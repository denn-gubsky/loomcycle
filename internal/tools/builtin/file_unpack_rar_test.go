package builtin

import (
	"encoding/binary"
	"hash/crc32"
)

// RAR fixtures are written here, in test code: RAR cannot be written with
// anything free, but its stored (uncompressed) form is a short documented
// layout (RARLAB's technote for RAR 5, and the RAR 2.9 one for RAR 4). Every
// fixture stores its files, so what the tests check is the walk, the names
// and the bounds around the decoder, not decompression. bsdtar (libarchive)
// lists and extracts what these writers produce, an independent check that
// they are real archives.

type rarEnt struct {
	name    string
	body    []byte
	dir     bool
	symlink string // the target, for a symbolic link
	// encrypted marks the file's data encrypted (its bytes are left as
	// they are: no reader without the password gets that far).
	encrypted bool
	// unknownSize leaves the unpacked size out of the header (RAR 5 only).
	unknownSize bool
}

type rarOpts struct {
	solid, volume, encryptedHeaders bool
}

func rarVintBytes(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

// rar5Block is one RAR 5 header: CRC32, size, type, flags, the optional
// extra-area and data sizes, the type's fields and the extra area. dataLen
// -1 is a header with no data area; a file header always has one, if empty
// (libarchive refuses a file header without).
func rar5Block(typ, hflags uint64, fields, extra []byte, dataLen int) []byte {
	if len(extra) > 0 {
		hflags |= 0x1
	}
	if dataLen >= 0 {
		hflags |= 0x2
	}
	body := rarVintBytes(typ)
	body = append(body, rarVintBytes(hflags)...)
	if len(extra) > 0 {
		body = append(body, rarVintBytes(uint64(len(extra)))...)
	}
	if dataLen >= 0 {
		body = append(body, rarVintBytes(uint64(dataLen))...)
	}
	body = append(body, fields...)
	body = append(body, extra...)
	sized := append(rarVintBytes(uint64(len(body))), body...)
	return append(le32(crc32.ChecksumIEEE(sized)), sized...)
}

func rar5Record(typ uint64, data []byte) []byte {
	rec := append(rarVintBytes(typ), data...)
	return append(rarVintBytes(uint64(len(rec))), rec...)
}

// rar5Archive writes a RAR 5 archive storing ents.
func rar5Archive(o rarOpts, ents ...rarEnt) []byte {
	out := []byte("Rar!\x1a\x07\x01\x00")
	if o.encryptedHeaders {
		enc := append(rarVintBytes(0), rarVintBytes(0)...)
		enc = append(enc, 15)
		enc = append(enc, make([]byte, 16)...)
		out = append(out, rar5Block(4, 0, enc, nil, -1)...)
	}
	var aflags uint64
	if o.volume {
		aflags |= 0x1
	}
	if o.solid {
		aflags |= 0x4
	}
	out = append(out, rar5Block(1, 0, rarVintBytes(aflags), nil, -1)...)
	for _, e := range ents {
		data := e.body
		var fflags uint64 = 0x4 // CRC32 present
		attr := uint64(0o100644)
		var extra []byte
		switch {
		case e.dir:
			fflags, attr, data = 0x1, 0o40755, nil
		case e.symlink != "":
			attr, data = 0o120777, nil
			redir := append(rarVintBytes(1), rarVintBytes(0)...)
			redir = append(redir, rarVintBytes(uint64(len(e.symlink)))...)
			extra = rar5Record(5, append(redir, e.symlink...))
		}
		if e.encrypted {
			rec := append(rarVintBytes(0), rarVintBytes(0)...)
			rec = append(rec, 15)
			rec = append(rec, make([]byte, 32)...)
			extra = append(extra, rar5Record(1, rec)...)
		}
		if e.unknownSize {
			fflags |= 0x8
		}
		fields := rarVintBytes(fflags)
		fields = append(fields, rarVintBytes(uint64(len(data)))...)
		fields = append(fields, rarVintBytes(attr)...)
		if fflags&0x4 != 0 {
			fields = append(fields, le32(crc32.ChecksumIEEE(data))...)
		}
		fields = append(fields, rarVintBytes(0)...) // compression: version 0, stored
		fields = append(fields, rarVintBytes(1)...) // host OS: Unix
		fields = append(fields, rarVintBytes(uint64(len(e.name)))...)
		fields = append(fields, e.name...)
		out = append(out, rar5Block(2, 0, fields, extra, len(data))...)
		out = append(out, data...)
	}
	var end uint64
	if o.volume {
		end = 0x1 // not the last volume
	}
	return append(out, rar5Block(5, 0, rarVintBytes(end), nil, -1)...)
}

// rar4Block is one RAR 1.5-4 header: its CRC (the low 16 bits of the CRC32 of
// the rest), type, flags, size and fields.
func rar4Block(typ byte, flags uint16, fields []byte) []byte {
	hdr := []byte{typ}
	hdr = append(hdr, le16(flags)...)
	hdr = append(hdr, le16(uint16(7+len(fields)))...)
	hdr = append(hdr, fields...)
	return append(le16(uint16(crc32.ChecksumIEEE(hdr))), hdr...)
}

// rar4Archive writes a RAR 4 archive storing ents.
func rar4Archive(o rarOpts, ents ...rarEnt) []byte {
	out := []byte("Rar!\x1a\x07\x00")
	var aflags uint16
	if o.volume {
		aflags |= 0x0001
	}
	if o.solid {
		aflags |= 0x0008
	}
	if o.encryptedHeaders {
		aflags |= 0x0080
	}
	out = append(out, rar4Block(0x73, aflags, make([]byte, 6))...)
	for _, e := range ents {
		data := e.body
		flags := uint16(0x8000)
		attr := uint32(0o100644)
		switch {
		case e.dir:
			flags |= 0x00e0
			attr, data = 0o40755, nil
		case e.symlink != "":
			attr, data = 0o120777, []byte(e.symlink)
		}
		if e.encrypted {
			flags |= 0x0004
		}
		f := le32(uint32(len(data)))
		f = append(f, le32(uint32(len(data)))...)
		f = append(f, 3) // host OS: Unix
		f = append(f, le32(crc32.ChecksumIEEE(data))...)
		f = append(f, le32(0x5a210000)...) // a DOS time
		f = append(f, 29, 0x30)            // version 2.9, stored
		f = append(f, le16(uint16(len(e.name)))...)
		f = append(f, le32(attr)...)
		f = append(f, e.name...)
		out = append(out, rar4Block(0x74, flags, f)...)
		out = append(out, data...)
	}
	return append(out, rar4Block(0x7b, 0, nil)...)
}
