package extract

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
)

// The checks below read the archive's own records (end of central
// directory, central directory, local file headers, data descriptors)
// instead of trusting archive/zip's view, which allows several central
// directory entries to point at the same compressed bytes: a few kilobytes
// of real data can then be listed as gigabytes of distinct files. An archive
// is accepted only when every entry's local record lies in its own,
// non-overlapping span before the central directory and names the same
// path as its central directory entry.

const (
	sigEOCD         = 0x06054b50
	sigZip64EOCD    = 0x06064b50
	sigZip64Locator = 0x07064b50
	sigCentral      = 0x02014b50
	sigLocal        = 0x04034b50
	sigDescriptor   = 0x08074b50
	eocdLen         = 22
	centralLen      = 46
	localLen        = 30
	zip64LocatorLen = 20
	zip64EOCDLen    = 56
	maxCommentLen   = 0xffff
	flagDescriptor  = 0x8
	maxCentralBytes = 16 << 20
)

// cdEntry is one central directory entry as recorded in the archive.
type cdEntry struct {
	name         []byte
	flags        uint16
	compressed   uint64
	uncompressed uint64
	offset       uint64 // of the local file header
	zip64        bool   // sizes came from the zip64 extra field
}

// readCentral parses the end of central directory record(s) and the
// central directory of the archive r (of size size).
func readCentral(r io.ReaderAt, size int64, maxEntries int) (entries []cdEntry, cdOffset uint64, err error) {
	tailLen := min(size, eocdLen+maxCommentLen)
	tail := make([]byte, tailLen)
	if _, err := r.ReadAt(tail, size-tailLen); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	at := -1
	for i := len(tail) - eocdLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == sigEOCD {
			at = i
			break
		}
	}
	if at < 0 {
		return nil, 0, unsafe("not a valid ZIP archive (no end of central directory record)")
	}
	eocd := tail[at:]
	if eocdLen+int(binary.LittleEndian.Uint16(eocd[20:])) != len(eocd) {
		return nil, 0, unsafe("malformed end of central directory")
	}
	count := uint64(binary.LittleEndian.Uint16(eocd[10:]))
	cdSize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	cdOffset = uint64(binary.LittleEndian.Uint32(eocd[16:]))
	eocdPos := size - tailLen + int64(at)
	cdEnd := uint64(eocdPos)
	if count == 0xffff || cdSize == 0xffffffff || cdOffset == 0xffffffff {
		// Zip64: the locator sits right before the EOCD record.
		if eocdPos < zip64LocatorLen {
			return nil, 0, unsafe("truncated zip64 records")
		}
		loc := make([]byte, zip64LocatorLen)
		if _, err := r.ReadAt(loc, eocdPos-zip64LocatorLen); err != nil {
			return nil, 0, err
		}
		if binary.LittleEndian.Uint32(loc) != sigZip64Locator {
			return nil, 0, unsafe("missing zip64 locator")
		}
		recOff := binary.LittleEndian.Uint64(loc[8:])
		locOff := uint64(eocdPos - zip64LocatorLen)
		if locOff < zip64EOCDLen || recOff > locOff-zip64EOCDLen {
			return nil, 0, unsafe("zip64 record out of range")
		}
		rec := make([]byte, zip64EOCDLen)
		if _, err := r.ReadAt(rec, int64(recOff)); err != nil {
			return nil, 0, err
		}
		if binary.LittleEndian.Uint32(rec) != sigZip64EOCD {
			return nil, 0, unsafe("bad zip64 end of central directory")
		}
		if binary.LittleEndian.Uint64(rec[4:]) != locOff-recOff-12 {
			return nil, 0, unsafe("malformed zip64 end of central directory")
		}
		count = binary.LittleEndian.Uint64(rec[32:])
		cdSize = binary.LittleEndian.Uint64(rec[40:])
		cdOffset = binary.LittleEndian.Uint64(rec[48:])
		cdEnd = recOff
	}
	if cdOffset > cdEnd || cdSize != cdEnd-cdOffset {
		return nil, 0, unsafe("central directory out of range")
	}
	if maxEntries < 0 || count > uint64(maxEntries) {
		return nil, 0, unsafe("archive exceeds the remaining build entry limit (%d entries)", maxEntries)
	}
	if cdSize > maxCentralBytes {
		return nil, 0, unsafe("central directory exceeds the metadata limit")
	}
	if count > cdSize/centralLen {
		return nil, 0, unsafe("central directory entry count does not fit")
	}
	cd := make([]byte, cdSize)
	if _, err := r.ReadAt(cd, int64(cdOffset)); err != nil {
		return nil, 0, err
	}
	entries = make([]cdEntry, 0, count)
	p := 0
	for uint64(len(entries)) < count {
		if p+centralLen > len(cd) || binary.LittleEndian.Uint32(cd[p:]) != sigCentral {
			return nil, 0, unsafe("malformed central directory")
		}
		h := cd[p:]
		nameLen := int(binary.LittleEndian.Uint16(h[28:]))
		extraLen := int(binary.LittleEndian.Uint16(h[30:]))
		commentLen := int(binary.LittleEndian.Uint16(h[32:]))
		end := p + centralLen + nameLen + extraLen + commentLen
		if end > len(cd) {
			return nil, 0, unsafe("malformed central directory")
		}
		e := cdEntry{
			name:         bytes.Clone(h[centralLen : centralLen+nameLen]),
			flags:        binary.LittleEndian.Uint16(h[8:]),
			compressed:   uint64(binary.LittleEndian.Uint32(h[20:])),
			uncompressed: uint64(binary.LittleEndian.Uint32(h[24:])),
			offset:       uint64(binary.LittleEndian.Uint32(h[42:])),
		}
		if err := e.applyZip64(h[centralLen+nameLen : centralLen+nameLen+extraLen]); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
		p = end
	}
	if p != len(cd) {
		return nil, 0, unsafe("central directory entry count mismatch")
	}
	return entries, cdOffset, nil
}

// applyZip64 replaces 0xffffffff fields with the zip64 extra field values
// (uncompressed, compressed, offset, in that order, each only if needed).
func (e *cdEntry) applyZip64(extra []byte) error {
	need := e.uncompressed == 0xffffffff || e.compressed == 0xffffffff || e.offset == 0xffffffff
	for len(extra) >= 4 {
		id := binary.LittleEndian.Uint16(extra)
		n := int(binary.LittleEndian.Uint16(extra[2:]))
		if 4+n > len(extra) {
			return unsafe("malformed extra field")
		}
		body := extra[4 : 4+n]
		extra = extra[4+n:]
		if id != 0x0001 {
			continue
		}
		read := func(v *uint64) error {
			if *v != 0xffffffff {
				return nil
			}
			if len(body) < 8 {
				return unsafe("truncated zip64 extra field")
			}
			*v = binary.LittleEndian.Uint64(body)
			body = body[8:]
			return nil
		}
		for _, v := range []*uint64{&e.uncompressed, &e.compressed, &e.offset} {
			if err := read(v); err != nil {
				return err
			}
		}
		e.zip64 = true
		need = false
	}
	if need {
		return unsafe("missing zip64 extra field")
	}
	return nil
}

// checkLayout verifies that the local records of entries do not overlap
// each other or the central directory, and that each local header names
// the same path as its central directory entry.
func checkLayout(r io.ReaderAt, entries []cdEntry, cdOffset uint64) error {
	type span struct{ start, end uint64 }
	spans := make([]span, 0, len(entries))
	hdr := make([]byte, localLen)
	for _, e := range entries {
		if e.offset > cdOffset || cdOffset-e.offset < localLen {
			return unsafe("entry %q starts outside the file data", clip(string(e.name)))
		}
		if _, err := r.ReadAt(hdr, int64(e.offset)); err != nil {
			return unsafe("entry %q has an unreadable local header", clip(string(e.name)))
		}
		if binary.LittleEndian.Uint32(hdr) != sigLocal {
			return unsafe("entry %q has no local file header", clip(string(e.name)))
		}
		nameLen := uint64(binary.LittleEndian.Uint16(hdr[26:]))
		extraLen := uint64(binary.LittleEndian.Uint16(hdr[28:]))
		dataStart := e.offset + localLen + nameLen + extraLen
		if dataStart > cdOffset {
			return unsafe("entry %q overlaps the central directory", clip(string(e.name)))
		}
		local := make([]byte, nameLen)
		if _, err := r.ReadAt(local, int64(e.offset+localLen)); err != nil {
			return unsafe("entry %q has an unreadable local header", clip(string(e.name)))
		}
		if !bytes.Equal(local, e.name) {
			return unsafe("entry %q has a different name in its local header", clip(string(e.name)))
		}
		if e.compressed > cdOffset-dataStart {
			return unsafe("entry %q overlaps the central directory", clip(string(e.name)))
		}
		end := dataStart + e.compressed
		if e.flags&flagDescriptor != 0 {
			// crc32 + sizes (4+4 or 8+8), optionally preceded by a signature.
			n := uint64(12)
			if e.zip64 {
				n = 20
			}
			sig := make([]byte, 4)
			if end+4 <= cdOffset {
				if _, err := r.ReadAt(sig, int64(end)); err == nil && binary.LittleEndian.Uint32(sig) == sigDescriptor {
					n += 4
				}
			}
			if n > cdOffset-end {
				return unsafe("entry %q overlaps the central directory", clip(string(e.name)))
			}
			end += n
		}
		spans = append(spans, span{e.offset, end})
	}
	slices.SortFunc(spans, func(a, b span) int {
		switch {
		case a.start < b.start:
			return -1
		case a.start > b.start:
			return 1
		}
		return 0
	})
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return unsafe("archive entries overlap (several entries share the same data)")
		}
	}
	return nil
}
