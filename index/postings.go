package index

import (
	"errors"
	"fmt"
	"github.com/balzaczyy/golucene/codec"
	"github.com/balzaczyy/golucene/store"
	"github.com/balzaczyy/golucene/util"
	"io"
	"log"
	"sort"
)

type FieldsProducer interface {
	Fields
	io.Closer
}

// BlockTreeTermsReader.java

const (
	BTT_OUTPUT_FLAGS_NUM_BITS = 2
	BTT_OUTPUT_FLAG_IS_FLOOR  = 1
	BTT_OUTPUT_FLAG_HAS_TERMS = 2

	BTT_EXTENSION           = "tim"
	BTT_CODEC_NAME          = "BLOCK_TREE_TERMS_DICT"
	BTT_VERSION_START       = 0
	BTT_VERSION_APPEND_ONLY = 1
	BTT_VERSION_CURRENT     = BTT_VERSION_APPEND_ONLY

	BTT_INDEX_EXTENSION           = "tip"
	BTT_INDEX_CODEC_NAME          = "BLOCK_TREE_TERMS_INDEX"
	BTT_INDEX_VERSION_START       = 0
	BTT_INDEX_VERSION_APPEND_ONLY = 1
	BTT_INDEX_VERSION_CURRENT     = BTT_INDEX_VERSION_APPEND_ONLY
)

/* A block-based terms index and dictionary that assigns
terms to variable length blocks according to how they
share prefixes. The terms index is a prefix trie
whose leaves are term blocks. The advantage of this
approach is that seekExact is often able to
determine a term cannot exist without doing any IO, and
intersection with Automata is very fast. NOte that this
terms dictionary has its own fixed terms index (ie, it
does not support a pluggable terms index
implementation).

NOTE: this terms dictionary does not support
index divisor when opening an IndexReader. Instead, you
can change the min/maxItemsPerBlock during indexing.

The data strucure used by this implementation is very
similar to a [burst trie]
(http://citeseer.ist.psu.edu/viewdoc/summary?doi=10.1.1.18.3499),
but with added logic to break up too-large blocks of all
terms sharing a given prefix into smaller ones.

Use CheckIndex with the -verbose
option to see summary statistics on the blocks in the
dictionary. */
type BlockTreeTermsReader struct {
	// Open input to the main terms dict file (_X.tib)
	in store.IndexInput
	// Reads the terms dict entries, to gather state to
	// produce DocsEnum on demand
	postingsReader PostingsReaderBase
	fields         map[string]FieldReader
	// File offset where the directory starts in the terms file.
	dirOffset int64
	// File offset where the directory starts in the index file.
	indexDirOffset int64
	segment        string
	version        int
}

func newBlockTreeTermsReader(dir store.Directory, fieldInfos FieldInfos, info SegmentInfo,
	postingsReader PostingsReaderBase, ctx store.IOContext,
	segmentSuffix string, indexDivisor int) (p FieldsProducer, err error) {
	log.Print("Initializing BlockTreeTermsReader...")
	fp := &BlockTreeTermsReader{
		postingsReader: postingsReader,
		fields:         make(map[string]FieldReader),
		segment:        info.name,
	}
	fp.in, err = dir.OpenInput(util.SegmentFileName(info.name, segmentSuffix, BTT_EXTENSION), ctx)
	if err != nil {
		return fp, err
	}

	success := false
	var indexIn store.IndexInput
	defer func() {
		if !success {
			log.Print("Failed to initialize BlockTreeTermsReader.")
			if err != nil {
				log.Print("DEBUG ", err)
			}
			// this.close() will close in:
			util.CloseWhileSuppressingError(indexIn, fp)
		}
	}()

	fp.version, err = fp.readHeader(fp.in)
	if err != nil {
		return fp, err
	}
	log.Printf("Version: %v", fp.version)

	if indexDivisor != -1 {
		indexIn, err = dir.OpenInput(util.SegmentFileName(info.name, segmentSuffix, BTT_INDEX_EXTENSION), ctx)
		if err != nil {
			return fp, err
		}

		indexVersion, err := fp.readIndexHeader(indexIn)
		if err != nil {
			return fp, err
		}
		log.Printf("Index version: %v", indexVersion)
		if int(indexVersion) != fp.version {
			return fp, errors.New(fmt.Sprintf("mixmatched version files: %v=%v,%v=%v", fp.in, fp.version, indexIn, indexVersion))
		}
	}

	// Have PostingsReader init itself
	postingsReader.Init(fp.in)

	// Read per-field details
	fp.seekDir(fp.in, fp.dirOffset)
	if indexDivisor != -1 {
		fp.seekDir(indexIn, fp.indexDirOffset)
	}

	numFields, err := fp.in.ReadVInt()
	if err != nil {
		return fp, err
	}
	log.Printf("Fields number: %v", numFields)
	if numFields < 0 {
		return fp, errors.New(fmt.Sprintf("invalid numFields: %v (resource=%v)", numFields, fp.in))
	}

	for i := int32(0); i < numFields; i++ {
		log.Printf("Next field...")
		field, err := fp.in.ReadVInt()
		if err != nil {
			return fp, err
		}
		log.Printf("Field: %v", field)

		numTerms, err := fp.in.ReadVLong()
		if err != nil {
			return fp, err
		}
		// assert numTerms >= 0
		log.Printf("Terms number: %v", numTerms)

		numBytes, err := fp.in.ReadVInt()
		if err != nil {
			return fp, err
		}
		log.Printf("Bytes number: %v", numBytes)

		rootCode := make([]byte, numBytes)
		err = fp.in.ReadBytes(rootCode)
		if err != nil {
			return fp, err
		}
		fieldInfo := fieldInfos.byNumber[field]
		// assert fieldInfo != nil
		var sumTotalTermFreq int64
		if fieldInfo.indexOptions == INDEX_OPT_DOCS_ONLY {
			sumTotalTermFreq = -1
		} else {
			sumTotalTermFreq, err = fp.in.ReadVLong()
			if err != nil {
				return fp, err
			}
		}
		sumDocFreq, err := fp.in.ReadVLong()
		if err != nil {
			return fp, err
		}
		docCount, err := fp.in.ReadVInt()
		if err != nil {
			return fp, err
		}
		log.Printf("DocCount: %v", docCount)
		if docCount < 0 || docCount > info.docCount { // #docs with field must be <= #docs
			return fp, errors.New(fmt.Sprintf(
				"invalid docCount: %v maxDoc: %v (resource=%v)",
				docCount, info.docCount, fp.in))
		}
		if sumDocFreq < int64(docCount) { // #postings must be >= #docs with field
			return fp, errors.New(fmt.Sprintf(
				"invalid sumDocFreq: %v docCount: %v (resource=%v)",
				sumDocFreq, docCount, fp.in))
		}
		if sumTotalTermFreq != -1 && sumTotalTermFreq < sumDocFreq { // #positions must be >= #postings
			return fp, errors.New(fmt.Sprintf(
				"invalid sumTotalTermFreq: %v sumDocFreq: %v (resource=%v)",
				sumTotalTermFreq, sumDocFreq, fp.in))
		}

		var indexStartFP int64
		if indexDivisor != -1 {
			indexStartFP, err = indexIn.ReadVLong()
			if err != nil {
				return fp, err
			}
		}
		log.Printf("indexStartFP: %v", indexStartFP)
		if _, ok := fp.fields[fieldInfo.name]; ok {
			return fp, errors.New(fmt.Sprintf(
				"duplicate field: %v (resource=%v)", fieldInfo.name, fp.in))
		}
		fp.fields[fieldInfo.name], err = newFieldReader(fp,
			fieldInfo, numTerms, rootCode, sumTotalTermFreq,
			sumDocFreq, docCount, indexStartFP, indexIn)
		if err != nil {
			return fp, err
		}
		log.Print("DEBUG field processed.")
	}

	if indexDivisor != -1 {
		err = indexIn.Close()
		if err != nil {
			return fp, err
		}
	}

	success = true

	return fp, nil
}

func asInt(n int32, err error) (n2 int, err2 error) {
	return int(n), err
}

func (r *BlockTreeTermsReader) readHeader(input store.IndexInput) (version int, err error) {
	version, err = asInt(codec.CheckHeader(input, BTT_CODEC_NAME, BTT_VERSION_START, BTT_VERSION_CURRENT))
	if err != nil {
		return int(version), err
	}
	if version < BTT_VERSION_APPEND_ONLY {
		r.dirOffset, err = input.ReadLong()
		if err != nil {
			return int(version), err
		}
	}
	return int(version), nil
}

func (r *BlockTreeTermsReader) readIndexHeader(input store.IndexInput) (version int, err error) {
	version, err = asInt(codec.CheckHeader(input, BTT_INDEX_CODEC_NAME, BTT_INDEX_VERSION_START, BTT_INDEX_VERSION_CURRENT))
	if err != nil {
		return version, err
	}
	if version < BTT_INDEX_VERSION_APPEND_ONLY {
		r.indexDirOffset, err = input.ReadLong()
		if err != nil {
			return version, err
		}
	}
	return version, nil
}

func (r *BlockTreeTermsReader) seekDir(input store.IndexInput, dirOffset int64) (err error) {
	log.Printf("Seeking to: %v", dirOffset)
	if r.version >= BTT_INDEX_VERSION_APPEND_ONLY {
		input.Seek(input.Length() - 8)
		if dirOffset, err = input.ReadLong(); err != nil {
			return err
		}
	}
	input.Seek(dirOffset)
	return nil
}

func (r *BlockTreeTermsReader) Terms(field string) Terms {
	ans := r.fields[field]
	return &ans
}

func (r *BlockTreeTermsReader) Close() error {
	defer func() {
		// Clear so refs to terms index is GCable even if
		// app hangs onto us:
		r.fields = make(map[string]FieldReader)
	}()
	return util.Close(r.in, r.postingsReader)
}

type FieldReader struct {
	*BlockTreeTermsReader // inner class

	numTerms         int64
	fieldInfo        FieldInfo
	sumTotalTermFreq int64
	sumDocFreq       int64
	docCount         int32
	indexStartFP     int64
	rootBlockFP      int64
	rootCode         []byte
	index            *util.FST
}

func newFieldReader(owner *BlockTreeTermsReader,
	fieldInfo FieldInfo, numTerms int64, rootCode []byte,
	sumTotalTermFreq, sumDocFreq int64, docCount int32, indexStartFP int64,
	indexIn store.IndexInput) (r FieldReader, err error) {
	log.Print("Initializing FieldReader...")
	if numTerms <= 0 {
		panic("assert fail")
	}
	// assert numTerms > 0
	r = FieldReader{
		BlockTreeTermsReader: owner,
		fieldInfo:            fieldInfo,
		numTerms:             numTerms,
		sumTotalTermFreq:     sumTotalTermFreq,
		sumDocFreq:           sumDocFreq,
		docCount:             docCount,
		indexStartFP:         indexStartFP,
		rootCode:             rootCode,
	}
	log.Printf("BTTR: seg=%v field=%v rootBlockCode=%v divisor=",
		owner.segment, fieldInfo.name, rootCode)

	in := store.NewByteArrayDataInput(rootCode)
	n, err := in.ReadVLong()
	if err != nil {
		return r, err
	}
	r.rootBlockFP = int64(uint64(n) >> BTT_OUTPUT_FLAGS_NUM_BITS)

	if indexIn != nil {
		clone := indexIn.Clone()
		log.Printf("start=%v field=%v", indexStartFP, fieldInfo.name)
		clone.Seek(indexStartFP)
		r.index, err = util.LoadFST(clone, util.ByteSequenceOutputsSingleton())
	}

	return r, err
}

func (r *FieldReader) Iterator(reuse TermsEnum) TermsEnum {
	return newSegmentTermsEnum(r)
}

func (r *FieldReader) SumTotalTermFreq() int64 {
	return r.sumTotalTermFreq
}

func (r *FieldReader) SumDocFreq() int64 {
	return r.sumDocFreq
}

func (r *FieldReader) DocCount() int {
	return int(r.docCount)
}

// BlockTreeTermsReader.java/SegmentTermsEnum
// Iterates through terms in this field
type SegmentTermsEnum struct {
	*TermsEnumImpl
	*FieldReader

	in store.IndexInput

	stack        []*segmentTermsEnumFrame
	staticFrame  *segmentTermsEnumFrame
	currentFrame *segmentTermsEnumFrame
	termExists   bool

	targetBeforeCurrentLength int

	// What prefix of the current term was present in the index:
	scratchReader *store.ByteArrayDataInput

	// What prefix of the current term was present in the index:
	validIndexPrefix int

	// assert only:
	eof bool

	term      []byte
	fstReader util.BytesReader

	arcs []*util.Arc

	fstOutputs util.Outputs
}

func newSegmentTermsEnum(r *FieldReader) *SegmentTermsEnum {
	ans := &SegmentTermsEnum{
		FieldReader:   r,
		stack:         make([]*segmentTermsEnumFrame, 0),
		scratchReader: store.NewEmptyByteArrayDataInput(),
		term:          make([]byte, 0),
		arcs:          make([]*util.Arc, 1),
		fstOutputs:    util.ByteSequenceOutputsSingleton(),
	}
	ans.TermsEnumImpl = newTermsEnumImpl(ans)
	log.Println("BTTR.init seg=%v", r.segment)

	// Used to hold seek by TermState, or cached seek
	ans.staticFrame = newFrame(ans, -1)

	if r.index != nil {
		ans.fstReader = r.index.BytesReader()
	}

	// Init w/ root block; don't use index since it may
	// not (and need not) have been loaded
	for i, _ := range ans.arcs {
		ans.arcs[i] = &util.Arc{}
	}

	ans.currentFrame = ans.staticFrame
	var arc *util.Arc
	if r.index != nil {
		arc = r.index.FirstArc(ans.arcs[0])
		// Empty string prefix must have an output in the index!
		if !arc.IsFinal() {
			panic("assert fail")
		}
	}
	ans.currentFrame = ans.staticFrame
	ans.validIndexPrefix = 0
	log.Printf("init frame state %v", ans.currentFrame.ord)
	ans.printSeekState()

	// ans.computeBlockStats()

	return ans
}

func (e *SegmentTermsEnum) initIndexInput() {
	if e.in == nil {
		e.in = e.FieldReader.BlockTreeTermsReader.in.Clone()
	}
}

func (e *SegmentTermsEnum) frame(ord int) *segmentTermsEnumFrame {
	if ord == len(e.stack) {
		e.stack = append(e.stack, newFrame(e, ord))
	} else if ord > len(e.stack) {
		// TODO over-allocate to ensure performance
		next := make([]*segmentTermsEnumFrame, 1+ord)
		copy(next, e.stack)
		for i := len(e.stack); i < len(next); i++ {
			next[i] = newFrame(e, i)
		}
		e.stack = next
	}
	if e.stack[ord].ord != ord {
		panic("assert fail")
	}
	return e.stack[ord]
}

func (e *SegmentTermsEnum) getArc(ord int) *util.Arc {
	if ord == len(e.arcs) {
		e.arcs = append(e.arcs, &util.Arc{})
	} else if ord > len(e.arcs) {
		// TODO over-allocate
		next := make([]*util.Arc, 1+ord)
		copy(next, e.arcs)
		for i := len(e.arcs); i < len(next); i++ {
			next[i] = &util.Arc{}
		}
		e.arcs = next
	}
	return e.arcs[ord]
}

func (e *SegmentTermsEnum) Comparator() sort.Interface {
	panic("not implemented yet")
}

// Pushes a frame we seek'd to
func (e *SegmentTermsEnum) pushFrame(arc *util.Arc, frameData []byte, length int) (f *segmentTermsEnumFrame, err error) {
	e.scratchReader.Reset(frameData)
	code, err := e.scratchReader.ReadVLong()
	if err != nil {
		return nil, err
	}
	fpSeek := int64(uint64(code) >> BTT_OUTPUT_FLAGS_NUM_BITS)
	f = e.frame(1 + e.currentFrame.ord)
	f.hasTerms = (code & BTT_OUTPUT_FLAG_HAS_TERMS) != 0
	f.hasTermsOrig = f.hasTerms
	f.isFloor = (code & BTT_OUTPUT_FLAG_IS_FLOOR) != 0
	if f.isFloor {
		f.setFloorData(e.scratchReader, frameData)
	}
	e.pushFrameAt(arc, fpSeek, length)
	return f, err
}

// Pushes next'd frame or seek'd frame; we later
// lazy-load the frame only when needed
func (e *SegmentTermsEnum) pushFrameAt(arc *util.Arc, fp int64, length int) (f *segmentTermsEnumFrame, err error) {
	f = e.frame(1 + e.currentFrame.ord)
	f.arc = arc
	if f.fpOrig == fp && f.nextEnt != -1 {
		log.Printf("      push reused frame ord=%v fp=%v isFloor?=%v hasTerms=%v pref=%v nextEnt=%v targetBeforeCurrentLength=%v term.length=%v vs prefix=%v",
			f.ord, f.fp, f.isFloor, f.hasTerms, e.term, f.nextEnt, e.targetBeforeCurrentLength, len(e.term), f.prefix)
		if f.prefix > e.targetBeforeCurrentLength {
			f.rewind()
		} else {
			log.Println("        skip rewind!")
		}
		if length != f.prefix {
			panic("assert fail")
		}
	} else {
		f.nextEnt = -1
		f.prefix = length
		f.state.termBlockOrd = 0
		f.fpOrig, f.fp = fp, fp
		f.lastSubFP = -1
		log.Printf("      push new frame ord=%v fp=%v hasTerms=%v isFloor=%v pref=%v",
			f.ord, f.fp, f.hasTerms, f.isFloor, brToString(e.term))
	}
	return f, nil
}

func (e *SegmentTermsEnum) SeekExact(target []byte) (ok bool, err error) {
	if e.index == nil {
		panic("terms index was not loaded")
	}

	if cap(e.term) <= len(target) {
		next := make([]byte, len(e.term), len(target))
		copy(next, e.term)
		e.term = next
	}

	e.eof = false
	log.Printf("BTTR.seekExact seg=%v target=%v:%v current=%v (exists?=%v) validIndexPrefix=%v",
		e.segment, e.fieldInfo.name, brToString(target), brToString(e.term), e.termExists, e.validIndexPrefix)
	e.printSeekState()

	var arc *util.Arc
	var targetUpto int
	var output []byte

	e.targetBeforeCurrentLength = e.currentFrame.ord

	// if e.currentFrame != e.staticFrame {
	if e.currentFrame.ord != e.staticFrame.ord {
		// We are already seek'd; find the common
		// prefix of new seek term vs current term and
		// re-use the corresponding seek state.  For
		// example, if app first seeks to foobar, then
		// seeks to foobaz, we can re-use the seek state
		// for the first 5 bytes.

		log.Printf("  re-use current seek state validIndexPrefix=%v", e.validIndexPrefix)

		arc = e.arcs[0]
		if !arc.IsFinal() {
			panic("assert fail")
		}
		output = arc.Output.([]byte)
		targetUpto = 0

		lastFrame := e.stack[0]
		if e.validIndexPrefix > len(e.term) {
			panic("assert fail")
		}

		targetLimit := len(target)
		if e.validIndexPrefix < targetLimit {
			targetLimit = e.validIndexPrefix
		}

		cmp := 0

		// TODO: reverse vLong byte order for better FST
		// prefix output sharing

		noOutputs := e.fstOutputs.NoOutput()

		// First compare up to valid seek frames:
		for targetUpto < targetLimit {
			cmp = int(e.term[targetUpto] - target[targetUpto])
			log.Printf("    cycle targetUpto=%v (vs limit=%v) cmp=%v (targetLabel=%c vs termLabel=%c) arc.output=%v output=%v",
				targetUpto, targetLimit, cmp, target[targetUpto], e.term[targetUpto], arc.Output, output)
			if cmp != 0 {
				break
			}

			arc = e.arcs[1+targetUpto]
			if arc.Label != int(target[targetUpto]) {
				log.Printf("FAIL: arc.label=%c targetLabel=%c", arc.Label, target[targetUpto])
				panic("assert fail")
			}
			if arc.Output != noOutputs {
				output = e.fstOutputs.Add(output, arc.Output).([]byte)
			}
			if arc.IsFinal() {
				lastFrame = e.stack[1+lastFrame.ord]
			}
			targetUpto++
		}

		if cmp == 0 {
			targetUptoMid := targetUpto

			// Second compare the rest of the term, but
			// don't save arc/output/frame; we only do this
			// to find out if the target term is before,
			// equal or after the current term
			targetLimit2 := len(target)
			if len(e.term) < targetLimit2 {
				targetLimit2 = len(e.term)
			}
			for targetUpto < targetLimit2 {
				cmp = int(e.term[targetUpto] - target[targetUpto])
				log.Printf("    cycle2 targetUpto=%v (vs limit=%v) cmp=%v (targetLabel=%c vs termLabel=%c)",
					targetUpto, targetLimit, cmp, target[targetUpto], e.term[targetUpto])
				if cmp != 0 {
					break
				}
				targetUpto++
			}

			if cmp == 0 {
				cmp = len(e.term) - len(target)
			}
			targetUpto = targetUptoMid
		}

		if cmp < 0 {
			// Common case: target term is after current
			// term, ie, app is seeking multiple terms
			// in sorted order
			log.Printf("  target is after current (shares prefixLen=%v); frame.ord=%v", targetUpto, lastFrame.ord)
			e.currentFrame = lastFrame
		} else if cmp > 0 {
			// Uncommon case: target term
			// is before current term; this means we can
			// keep the currentFrame but we must rewind it
			// (so we scan from the start)
			e.targetBeforeCurrentLength = 0
			log.Printf("  target is before current (shares prefixLen=%v); rewind frame ord=%v", targetUpto, lastFrame.ord)
			e.currentFrame = lastFrame
			e.currentFrame.rewind()
		} else {
			// Target is exactly the same as current term
			if len(e.term) != len(target) {
				panic("assert fail")
			}
			if e.termExists {
				log.Println("  target is same as current; return true")
				return true, nil
			} else {
				log.Println("  target is same as current but term doesn't exist")
			}
		}
	} else {
		e.targetBeforeCurrentLength = -1
		arc = e.index.FirstArc(e.arcs[0])

		// Empty string prefix must have an output (block) in the index!
		log.Println(arc)
		if !arc.IsFinal() || arc.Output == nil {
			panic("assert fail")
		}

		output = arc.Output.([]byte)

		e.currentFrame = e.staticFrame

		targetUpto = 0
		e.currentFrame, err = e.pushFrame(arc, e.fstOutputs.Add(output, arc.NextFinalOutput).([]byte), 0)
		if err != nil {
			return false, err
		}
	}

	log.Printf("  start index loop targetUpto=%v output=%v currentFrame.ord=%v targetBeforeCurrentLength=%v",
		targetUpto, output, e.currentFrame.ord, e.targetBeforeCurrentLength)

	for targetUpto < len(target) {
		targetLabel := int(target[targetUpto])
		nextArc, err := e.index.FindTargetArc(targetLabel, arc, e.getArc(1+targetUpto), e.fstReader)
		if err != nil {
			return false, err
		}
		if nextArc == nil {
			// Index is exhausted
			log.Printf("    index: index exhausted label=%c %x", targetLabel, targetLabel)

			e.validIndexPrefix = e.currentFrame.prefix

			e.currentFrame.scanToFloorFrame(target)

			if !e.currentFrame.hasTerms {
				e.termExists = false
				e.term = append(e.term, byte(targetLabel))
				log.Printf("  FAST NOT_FOUND term=%v", brToString(e.term))
				return false, nil
			}

			e.currentFrame.loadBlock()

			status, err := e.currentFrame.scanToTerm(target, true)
			if err != nil {
				return false, err
			}
			if status == SEEK_STATUS_FOUND {
				log.Printf("  return FOUND term=%v %v", utf8ToString(e.term), e.term)
				return true, nil
			} else {
				log.Printf("  got %v; return NOT_FOUND term=%v", status, brToString(e.term))
				return false, nil
			}
		} else {
			// Follow this arc
			arc = nextArc
			e.term[targetUpto] = byte(targetLabel)
			// Aggregate output as we go:
			if arc.Output == nil {
				panic("assert fail")
			}
			noOutputs := e.fstOutputs.NoOutput()
			if arc.Output != noOutputs {
				output = e.fstOutputs.Add(output, arc.Output).([]byte)
			}
			log.Printf("    index: follow label=%x arc.output=%v arc.nfo=%v",
				target[targetUpto], arc.Output, arc.NextFinalOutput)
			targetUpto++

			if arc.IsFinal() {
				log.Println("    arc is final!")
				e.currentFrame, err = e.pushFrame(arc, e.fstOutputs.Add(output, arc.NextFinalOutput).([]byte), targetUpto)
				if err != nil {
					return false, err
				}
				log.Printf("    curFrame.ord=%v hasTerms=%v", e.currentFrame.ord, e.currentFrame.hasTerms)
			}
		}
	}

	e.validIndexPrefix = e.currentFrame.prefix

	e.currentFrame.scanToFloorFrame(target)

	// Target term is entirely contained in the index:
	if !e.currentFrame.hasTerms {
		e.termExists = false
		e.term = e.term[0:targetUpto]
		log.Printf("  FAST NOT_FOUND term=%v", brToString(e.term))
		return false, nil
	}

	e.currentFrame.loadBlock()

	status, err := e.currentFrame.scanToTerm(target, true)
	if err != nil {
		return false, err
	}
	if status == SEEK_STATUS_FOUND {
		log.Printf("  return FOUND term=%v %v", utf8ToString(e.term), e.term)
		return true, nil
	} else {
		log.Printf("  got result %v; return NOT_FOUND term=%v", status, utf8ToString(e.term))
		return false, nil
	}
}

func (e *SegmentTermsEnum) SeekCeil(text []byte) SeekStatus {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) printSeekState() {
	if e.currentFrame == e.staticFrame {
		log.Println("  no prior seek")
	} else {
		log.Println("  prior seek state:")
		ord := 0
		isSeekFrame := true
		for {
			f := e.frame(ord)
			if f == nil {
				panic("assert fail")
			}
			prefix := e.term[0:f.prefix]
			if f.nextEnt == -1 {
				action := "(next)"
				if isSeekFrame {
					action = "(seek)"
				}
				fpOrigValue := ""
				if f.isFloor {
					fpOrigValue = fmt.Sprintf(" (fpOrig=%v", f.fpOrig)
				}
				code := (f.fp << BTT_OUTPUT_FLAGS_NUM_BITS)
				if f.hasTerms {
					code += BTT_OUTPUT_FLAG_HAS_TERMS
				}
				if f.isFloor {
					code += BTT_OUTPUT_FLAG_IS_FLOOR
				}
				log.Printf("    frame %v ord=%v fp=%v%v prefixLen=%v prefix=%v hasTerms=%v isFloor=%v code=%v isLastInFloor=%v mdUpto=%v tbOrd=%v",
					action, ord, f.fp, fpOrigValue, f.prefix, prefix, f.hasTerms, f.isFloor, code, f.isLastInFloor, f.metaDataUpto, f.getTermBlockOrd())
			} else {
				action := "(next, loaded)"
				if isSeekFrame {
					action = "(seek, loaded)"
				}
				fpOrigValue := ""
				if f.isFloor {
					fpOrigValue = fmt.Sprintf(" (fpOrig=%v", f.fpOrig)
				}
				code := (f.fp << BTT_OUTPUT_FLAGS_NUM_BITS)
				if f.hasTerms {
					code += BTT_OUTPUT_FLAG_HAS_TERMS
				}
				if f.isFloor {
					code += BTT_OUTPUT_FLAG_IS_FLOOR
				}
				log.Printf("    frame %v ord=%v fp=%v prefixLen=%v prefix=%v nextEnt=%v (of %v) hasTerms=%v isFloor=%v code=%v lastSubFP=%v isLastInFloor=%v mdUpto=%v tbOrd=%v",
					action, ord, f.fp, fpOrigValue, f.prefix, prefix, f.nextEnt, f.entCount, f.hasTerms, f.isFloor, code, f.lastSubFP, f.isLastInFloor, f.metaDataUpto, f.getTermBlockOrd())
			}
			if e.index != nil {
				if isSeekFrame && f.arc == nil {
					log.Printf("isSeekFrame=%v f.arc=%v", isSeekFrame, f.arc)
					panic("assert fail")
				}
				ret, err := util.GetFSTOutput(e.index, prefix)
				if err != nil {
					panic(err)
				}
				output := ret.([]byte)
				if output == nil {
					log.Println("      broken seek state: prefix is not final in index")
					panic("seek state is broken")
				} else if isSeekFrame && !f.isFloor {
					reader := store.NewByteArrayDataInput(output)
					codeOrig, _ := reader.ReadVLong()
					code := f.fp << BTT_OUTPUT_FLAGS_NUM_BITS
					if f.hasTerms {
						code += BTT_OUTPUT_FLAG_HAS_TERMS
					}
					if f.isFloor {
						code += BTT_OUTPUT_FLAG_IS_FLOOR
					}
					if codeOrig != code {
						log.Printf("      broken seek state: output code=%v doesn't match frame code=%v", codeOrig, code)
						panic("seek state is broken")
					}
				}
			}
			if f == e.currentFrame {
				break
			}
			if f.prefix == e.validIndexPrefix {
				isSeekFrame = false
			}
			ord++
		}
	}
}

func (e *SegmentTermsEnum) Next() (buf []byte, err error) {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) Term() []byte {
	if e.eof {
		panic("assert fail")
	}
	return e.term
}

func (e *SegmentTermsEnum) DocFreq() int {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) TotalTermFreq() int64 {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) DocsByFlags(skipDocs util.Bits, reuse DocsEnum, flags int) DocsEnum {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) DocsAndPositionsByFlags(skipDocs util.Bits, reuse DocsAndPositionsEnum, flags int) DocsAndPositionsEnum {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) SeekExactFromLast(target []byte, otherState TermState) error {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) TermState() TermState {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) SeekExactByPosition(ord int64) error {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) Ord() int64 {
	panic("not supported!")
}

type segmentTermsEnumFrame struct {
	// internal data structure
	*SegmentTermsEnum

	// Our index in stack[]:
	ord int

	hasTerms     bool
	hasTermsOrig bool
	isFloor      bool

	arc *util.Arc

	// File pointer where this block was loaded from
	fp     int64
	fpOrig int64
	fpEnd  int64

	suffixBytes    []byte
	suffixesReader store.ByteArrayDataInput

	statBytes   []byte
	statsReader store.ByteArrayDataInput

	floorData       []byte
	floorDataReader store.ByteArrayDataInput

	// Length of prefix shared by all terms in this block
	prefix int

	// Number of entries (term or sub-block) in this block
	entCount int

	// Which term we will next read, or -1 if the block
	// isn't loaded yet
	nextEnt int

	// True if this block is either not a floor block,
	// or, it's the last sub-block of a floor block
	isLastInFloor bool

	// True if all entries are terms
	isLeafBlock bool

	lastSubFP int64

	nextFloorLabel       int
	numFollowFloorBlocks int

	// Next term to decode metaData; we decode metaData
	// lazily so that scanning to find the matching term is
	// fast and only if you find a match and app wants the
	// stats or docs/positions enums, will we decode the
	// metaData
	metaDataUpto int

	state *BlockTermState

	startBytePos int
	suffix       int
	subCode      int
}

func newFrame(owner *SegmentTermsEnum, ord int) *segmentTermsEnumFrame {
	f := &segmentTermsEnumFrame{
		SegmentTermsEnum: owner,
		suffixBytes:      make([]byte, 128),
		statBytes:        make([]byte, 64),
		floorData:        make([]byte, 32),
		ord:              ord,
	}
	f.state = owner.postingsReader.NewTermState()
	f.state.totalTermFreq = -1
	return f
}

func (f *segmentTermsEnumFrame) setFloorData(in *store.ByteArrayDataInput, source []byte) {
	numBytes := len(source) - (in.Pos - 0)
	if numBytes > len(f.floorData) {
		// TODO over allocate
		f.floorData = make([]byte, numBytes)
	}
	copy(f.floorData, source[in.Pos:])
	f.floorDataReader.Reset(f.floorData)
	f.numFollowFloorBlocks, _ = asInt(f.floorDataReader.ReadVInt())
	b, _ := f.floorDataReader.ReadByte()
	f.nextFloorLabel = int(b)
	log.Printf("    setFloorData fpOrig=%v bytes=%v numFollowFloorBlocks=%v nextFloorLabel=%x",
		f.fpOrig, source[in.Pos:], f.numFollowFloorBlocks, f.nextFloorLabel)
}

func (f *segmentTermsEnumFrame) getTermBlockOrd() int {
	if f.isLeafBlock {
		return f.nextEnt
	} else {
		return f.state.termBlockOrd
	}
}

/* Does initial decode of next block of terms; this
   doesn't actually decode the docFreq, totalTermFreq,
   postings details (frq/prx offset, etc.) metadata;
   it just loads them as byte[] blobs which are then
   decoded on-demand if the metadata is ever requested
   for any term in this block.  This enables terms-only
   intensive consumes (eg certain MTQs, respelling) to
   not pay the price of decoding metadata they won't
   use. */
func (f *segmentTermsEnumFrame) loadBlock() (err error) {
	// Clone the IndexInput lazily, so that consumers
	// that just pull a TermsEnum to
	// seekExact(TermState) don't pay this cost:
	f.initIndexInput()

	if f.nextEnt != -1 {
		// Already loaded
		return
	}

	f.in.Seek(f.fp)
	code, err := asInt(f.in.ReadVInt())
	if err != nil {
		return err
	}
	f.entCount = int(uint(code) >> 1)
	if f.entCount <= 0 {
		panic("assert fail")
	}
	f.isLastInFloor = (code & 1) != 0
	if f.arc != nil && f.isLastInFloor && f.isFloor {
		panic("assert fail")
	}

	// TODO: if suffixes were stored in random-access
	// array structure, then we could do binary search
	// instead of linear scan to find target term; eg
	// we could have simple array of offsets

	// term suffixes:
	code, err = asInt(f.in.ReadVInt())
	f.isLeafBlock = (code & 1) != 0
	numBytes := int(uint(code) >> 1)
	if len(f.suffixBytes) < numBytes {
		f.suffixBytes = make([]byte, numBytes)
	}
	err = f.in.ReadBytes(f.suffixBytes)
	if err != nil {
		return err
	}
	f.suffixesReader.Reset(f.suffixBytes)

	if f.arc == nil {
		log.Printf("    loadBlock (next) fp=%v entCount=%v prefixLen=%v isLastInFloor=%v leaf?=%v",
			f.fp, f.entCount, f.prefix, f.isLastInFloor, f.isLeafBlock)
	} else {
		log.Printf("    loadBlock (seek) fp=%v entCount=%v prefixLen=%v hasTerms?=%v isFloor?=%v isLastInFloor=%v leaf?=%v",
			f.fp, f.entCount, f.prefix, f.hasTerms, f.isFloor, f.isLastInFloor, f.isLeafBlock)
	}

	// stats
	numBytes, err = asInt(f.in.ReadVInt())
	if err != nil {
		return nil
	}
	if len(f.statBytes) < numBytes {
		f.statBytes = make([]byte, numBytes)
	}
	err = f.in.ReadBytes(f.statBytes)
	if err != nil {
		return err
	}
	f.statsReader.Reset(f.statBytes)
	f.metaDataUpto = 0

	f.state.termBlockOrd = 0
	f.nextEnt = 0
	f.lastSubFP = -1

	// TODO: we could skip this if !hasTerms; but
	// that's rare so won't help much
	f.postingsReader.ReadTermsBlock(f.in, f.fieldInfo, f.state)

	// Sub-blocks of a single floor block are always
	// written one after another -- tail recurse:
	f.fpEnd = f.in.FilePointer()
	log.Printf("      fpEnd=%v", f.fpEnd)
	return nil
}

func (f *segmentTermsEnumFrame) rewind() {
	// Force reload:
	f.fp = f.fpOrig
	f.nextEnt = -1
	f.hasTerms = f.hasTermsOrig
	if f.isFloor {
		f.floorDataReader.Rewind()
		f.numFollowFloorBlocks, _ = asInt(f.floorDataReader.ReadVInt())
		b, _ := f.floorDataReader.ReadByte()
		f.nextFloorLabel = int(b)
	}
}

// TODO: make this array'd so we can do bin search?
// likely not worth it?  need to measure how many
// floor blocks we "typically" get
func (f *segmentTermsEnumFrame) scanToFloorFrame(target []byte) {
	if !f.isFloor || len(target) <= f.prefix {
		log.Printf("    scanToFloorFrame skip: isFloor=%v target.length=%v vs prefix=%v",
			f.isFloor, len(target), f.prefix)
		return
	}

	targetLabel := int(target[f.prefix])
	log.Printf("    scanToFloorFrame fpOrig=%v targetLabel=%x vs nextFloorLabel=%x numFollowFloorBlocks=%v",
		f.fpOrig, targetLabel, f.nextFloorLabel, f.numFollowFloorBlocks)
	if targetLabel < f.nextFloorLabel {
		log.Println("      already on correct block")
		return
	}

	if f.numFollowFloorBlocks == 0 {
		panic("assert fail")
	}

	panic("not implemented yet")
	// long newFP;
	//  while (true) {
	//    final long code = floorDataReader.readVLong();
	//    newFP = fpOrig + (code >>> 1);
	//    hasTerms = (code & 1) != 0;
	//    // if (DEBUG) {
	//    //   System.out.println("      label=" + toHex(nextFloorLabel) + " fp=" + newFP + " hasTerms?=" + hasTerms + " numFollowFloor=" + numFollowFloorBlocks);
	//    // }

	//    isLastInFloor = numFollowFloorBlocks == 1;
	//    numFollowFloorBlocks--;

	//    if (isLastInFloor) {
	//      nextFloorLabel = 256;
	//      // if (DEBUG) {
	//      //   System.out.println("        stop!  last block nextFloorLabel=" + toHex(nextFloorLabel));
	//      // }
	//      break;
	//    } else {
	//      nextFloorLabel = floorDataReader.readByte() & 0xff;
	//      if (targetLabel < nextFloorLabel) {
	//        // if (DEBUG) {
	//        //   System.out.println("        stop!  nextFloorLabel=" + toHex(nextFloorLabel));
	//        // }
	//        break;
	//      }
	//    }
	//  }

	//  if (newFP != fp) {
	//    // Force re-load of the block:
	//    // if (DEBUG) {
	//    //   System.out.println("      force switch to fp=" + newFP + " oldFP=" + fp);
	//    // }
	//    nextEnt = -1;
	//    fp = newFP;
	//  } else {
	//    // if (DEBUG) {
	//    //   System.out.println("      stay on same fp=" + newFP);
	//    // }
	//  }
}

// Used only by assert
func (f *segmentTermsEnumFrame) prefixMatches(target []byte) bool {
	panic("not implemented yet")
}

// NOTE: sets startBytePos/suffix as a side effect
func (f *segmentTermsEnumFrame) scanToTerm(target []byte, exactOnly bool) (status SeekStatus, err error) {
	if f.isLeafBlock {
		return f.scanToTermLeaf(target, exactOnly)
	}
	return f.scanToTermNonLeaf(target, exactOnly)
}

// Target's prefix matches this block's prefix; we
// scan the entries check if the suffix matches.
func (f *segmentTermsEnumFrame) scanToTermLeaf(target []byte, exactOnly bool) (status SeekStatus, err error) {
	log.Printf("    scanToTermLeaf: block fp=%v prefix=%v nextEnt=%v (of %v) target=%v term=%v",
		f.fp, f.prefix, f.nextEnt, f.entCount, brToString(target), brToString(f.term))
	if f.nextEnt == -1 {
		panic("assert fail")
	}

	f.termExists = true
	f.subCode = 0
	if f.nextEnt == f.entCount {
		if exactOnly {
			f.fillTerm()
		}
		return SEEK_STATUS_END, nil
	}

	if !f.prefixMatches(target) {
		panic("assert fail")
	}

	// Loop over each entry (term or sub-block) in this block:
	//nextTerm: while(nextEnt < entCount) {
	for {
		f.nextEnt++
		f.suffix, err = asInt(f.suffixesReader.ReadVInt())
		if err != nil {
			return 0, err
		}

		log.Printf("      cycle: term %v (of %v) suffix=%v",
			f.nextEnt-1, f.entCount, brToString(f.suffixBytes[f.suffixesReader.Pos:]))

		termLen := f.prefix + f.suffix
		f.startBytePos = f.suffixesReader.Pos
		f.suffixesReader.SkipBytes(f.suffix)

		targetLimit := termLen
		if len(target) < termLen {
			targetLimit = len(target)
		}
		targetPos := f.prefix

		// Loop over bytes in the suffix, comparing to
		// the target
		bytePos := f.startBytePos
		isDone := false
		for {
			var cmp int
			var stop bool
			if targetPos < targetLimit {
				cmp = int(f.suffixBytes[bytePos] - target[targetPos])
				bytePos++
				targetPos++
				stop = false
			} else {
				if targetPos != targetLimit {
					panic("assert fail")
				}
				cmp = termLen - len(target)
				stop = true
			}

			if cmp < 0 {
				// Current entry is still before the target;
				// keep scanning

				if f.nextEnt == f.entCount {
					if exactOnly {
						f.fillTerm()
					}
					// We are done scanning this block
					isDone = true
				}
				break
			} else if cmp > 0 {
				// // Done!  Current entry is after target --
				//     // return NOT_FOUND:
				//     fillTerm();

				//     if (!exactOnly && !termExists) {
				//       // We are on a sub-block, and caller wants
				//       // us to position to the next term after
				//       // the target, so we must recurse into the
				//       // sub-frame(s):
				//       currentFrame = pushFrame(null, currentFrame.lastSubFP, termLen);
				//       currentFrame.loadBlock();
				//       while (currentFrame.next()) {
				//         currentFrame = pushFrame(null, currentFrame.lastSubFP, term.length);
				//         currentFrame.loadBlock();
				//       }
				//     }

				//     //if (DEBUG) System.out.println("        not found");
				return SEEK_STATUS_NOT_FOUND, nil
			} else if stop {
				// Exact match!

				// This cannot be a sub-block because we
				// would have followed the index to this
				// sub-block from the start:

				if !f.termExists {
					panic("assert fail")
				}
				f.fillTerm()
				log.Println("        found!")
				return SEEK_STATUS_FOUND, nil
			}
		}
		if isDone {
			// double jump
			break
		}
	}

	// It is possible (and OK) that terms index pointed us
	// at this block, but, we scanned the entire block and
	// did not find the term to position to.  This happens
	// when the target is after the last term in the block
	// (but, before the next term in the index).  EG
	// target could be foozzz, and terms index pointed us
	// to the foo* block, but the last term in this block
	// was fooz (and, eg, first term in the next block will
	// bee fop).
	log.Println("      block end")
	if exactOnly {
		f.fillTerm()
	}

	// TODO: not consistent that in the
	// not-exact case we don't next() into the next
	// frame here
	return SEEK_STATUS_END, nil
}

// Target's prefix matches this block's prefix; we
// scan the entries check if the suffix matches.
func (f *segmentTermsEnumFrame) scanToTermNonLeaf(target []byte, exactOnly bool) (status SeekStatus, err error) {
	panic("not implemented yet")
}

func (f *segmentTermsEnumFrame) fillTerm() {
	termLength := f.prefix + f.suffix
	if len(f.term) < termLength {
		// TODO over-allocate
		next := make([]byte, termLength)
		copy(next, f.term)
		f.term = next
	}
	copy(f.term[f.prefix:f.prefix+f.suffix], f.suffixBytes[f.startBytePos:])
}

// for debugging
func brToString(b []byte) string {
	if b == nil {
		return "nil"
	} else {
		return fmt.Sprintf("%v %v", utf8ToString(b), b)
	}
}

// Simpler version of Lucene's own method
func utf8ToString(iso8859_1_buf []byte) string {
	buf := make([]rune, len(iso8859_1_buf))
	for i, b := range iso8859_1_buf {
		buf[i] = rune(b)
	}
	return string(buf)
}
