package util

import (
	"bytes"
	"fmt"
	// "log"
	"strconv"
	"strings"
)

const (
	SEGMENTS = "segments"
)

func FileNameFromGeneration(base, ext string, gen int64) string {
	// log.Printf("Filename from generation: %v, %v, %v", base, ext, gen)
	switch {
	case gen == -1:
		return ""
	case gen == 0:
		return SegmentFileName(base, "", ext)
	default:
		// assert gen > 0
		// The '6' part in the length is: 1 for '.', 1 for '_' and 4 as estimate
		// to the gen length as string (hopefully an upper limit so SB won't
		// expand in the middle.
		var buffer bytes.Buffer
		fmt.Fprintf(&buffer, "%v_%v", base, strconv.FormatInt(gen, 36))
		if len(ext) > 0 {
			buffer.WriteString(".")
			buffer.WriteString(ext)
		}
		return buffer.String()
	}
}

func SegmentFileName(name, suffix, ext string) string {
	if len(ext) > 0 || len(suffix) > 0 {
		// assert ext[0] != '.'
		var buffer bytes.Buffer
		buffer.WriteString(name)
		if len(suffix) > 0 {
			buffer.WriteString("_")
			buffer.WriteString(suffix)
		}
		if len(ext) > 0 {
			buffer.WriteString(".")
			buffer.WriteString(ext)
		}
		return buffer.String()
	}
	return name
}

func indexOfSegmentName(filename string) int {
	// If it is a .del file, there's an '_' after the first character
	if idx := strings.Index(filename[1:], "_"); idx >= 0 {
		return idx + 1
	}
	// If it's not, strip everything that's before the '.'
	return strings.Index(filename, ".")
}

func StripSegmentName(filename string) string {
	if idx := indexOfSegmentName(filename); idx != -1 {
		return filename[idx:]
	}
	return filename
}

func ParseSegmentName(filename string) string {
	if idx := indexOfSegmentName(filename); idx != -1 {
		return filename[0:idx]
	}
	return filename
}

func StripExtension(filename string) string {
	if idx := strings.Index(filename, "."); idx != -1 {
		return filename[0:idx]
	}
	return filename
}
