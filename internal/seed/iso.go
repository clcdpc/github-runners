// Package seed writes small ISO9660/Joliet NoCloud volumes without host mounts,
// COM, WSL, or an external ISO authoring program.
package seed

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"sort"
	"strings"
	"unicode/utf16"
)

const sector = 2048

func both16(b []byte, n uint16) {
	binary.LittleEndian.PutUint16(b, n)
	binary.BigEndian.PutUint16(b[2:], n)
}
func both32(b []byte, n uint32) {
	binary.LittleEndian.PutUint32(b, n)
	binary.BigEndian.PutUint32(b[4:], n)
}
func ucs(s string) []byte {
	b := []byte{}
	for _, c := range utf16.Encode([]rune(s)) {
		b = binary.BigEndian.AppendUint16(b, c)
	}
	return b
}
func record(name []byte, block, size uint32, directory bool) []byte {
	n := 33 + len(name)
	if n%2 != 0 {
		n++
	}
	b := make([]byte, n)
	b[0] = byte(n)
	both32(b[2:], block)
	both32(b[10:], size)
	copy(b[18:], []byte{126, 1, 1, 0, 0, 0, 0})
	if directory {
		b[25] = 2
	}
	both16(b[28:], 1)
	b[32] = byte(len(name))
	copy(b[33:], name)
	return b
}
func pathTable(block uint32, big bool) []byte {
	b := make([]byte, 10)
	b[0] = 1
	if big {
		binary.BigEndian.PutUint32(b[2:], block)
		binary.BigEndian.PutUint16(b[6:], 1)
	} else {
		binary.LittleEndian.PutUint32(b[2:], block)
		binary.LittleEndian.PutUint16(b[6:], 1)
	}
	return b
}

// ISO returns a CIDATA volume. Primary names are compatible with ISO level 2;
// Joliet preserves the exact NoCloud names (including hyphens and lowercase).
func ISO(files map[string][]byte) ([]byte, error) {
	if len(files) < 2 || len(files) > 20 {
		return nil, errors.New("seed needs 2..20 root files")
	}
	names := make([]string, 0, len(files))
	for name, b := range files {
		if name == "" || len(name) > 50 || strings.ContainsAny(name, "/\\\x00") || len(b) > 2<<20 {
			return nil, errors.New("unsafe seed file or seed too large")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	blocks := map[string]uint32{}
	next := uint32(25)
	for _, name := range names {
		blocks[name] = next
		next += max(1, (uint32(len(files[name]))+sector-1)/sector)
	}
	iso := make([]byte, int(next)*sector)
	for index, root := range []uint32{23, 24} {
		b := iso[(16+index)*sector:]
		b[0] = 1
		if index == 1 {
			b[0] = 2
		}
		copy(b[1:], "CD001")
		b[6] = 1
		for i := 8; i < 72; i++ {
			b[i] = ' '
		}
		if index == 0 {
			copy(b[8:], "CLC")
			copy(b[40:], "CIDATA")
		} else {
			copy(b[8:40], ucs("CLC             "))
			copy(b[40:72], ucs("CIDATA          "))
			copy(b[88:], "%/E")
		}
		both32(b[80:], next)
		both16(b[120:], 1)
		both16(b[124:], 1)
		both16(b[128:], sector)
		both32(b[132:], 10)
		binary.LittleEndian.PutUint32(b[140:], uint32(19+index*2))
		binary.BigEndian.PutUint32(b[148:], uint32(20+index*2))
		copy(b[156:], record([]byte{0}, root, sector, true))
		b[881] = 1
		copy(iso[(19+index*2)*sector:], pathTable(root, false))
		copy(iso[(20+index*2)*sector:], pathTable(root, true))
		dir := bytes.NewBuffer(nil)
		dir.Write(record([]byte{0}, root, sector, true))
		dir.Write(record([]byte{1}, root, sector, true))
		for i, name := range names {
			primary := []byte{byte('A' + i), ';', '1'}
			if index == 1 {
				primary = ucs(name)
			}
			dir.Write(record(primary, blocks[name], uint32(len(files[name])), false))
		}
		if dir.Len() > sector {
			return nil, errors.New("seed directory exceeds one sector")
		}
		copy(iso[int(root)*sector:], dir.Bytes())
	}
	b := iso[18*sector:]
	b[0] = 255
	copy(b[1:], "CD001")
	b[6] = 1
	for name, data := range files {
		copy(iso[int(blocks[name])*sector:], data)
	}
	return iso, nil
}
func Write(path string, files map[string][]byte) error {
	b, err := ISO(files)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}
