package seed

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoCloudJolietVolume(t *testing.T) {
	files := map[string][]byte{"user-data": []byte("#cloud-config\n"), "meta-data": []byte("instance-id: unique\n"), "network-config": bytes.Repeat([]byte("n"), 5000)}
	iso, err := ISO(files)
	if err != nil {
		t.Fatal(err)
	}
	if string(iso[16*sector+40:16*sector+46]) != "CIDATA" || string(iso[17*sector+88:17*sector+91]) != "%/E" {
		t.Fatal("invalid volume descriptors")
	}
	root := binary.LittleEndian.Uint32(iso[17*sector+158:])
	dir := iso[int(root)*sector : int(root+1)*sector]
	found := map[string]bool{}
	for offset := 0; dir[offset] != 0; {
		r := dir[offset : offset+int(dir[offset])]
		offset += len(r)
		if r[25]&2 != 0 {
			continue
		}
		name := ""
		for i := 0; i < int(r[32]); i += 2 {
			name += string(rune(binary.BigEndian.Uint16(r[33+i:])))
		}
		block := binary.LittleEndian.Uint32(r[2:])
		size := binary.LittleEndian.Uint32(r[10:])
		if !bytes.Equal(files[name], iso[int(block)*sector:int(block)*sector+int(size)]) {
			t.Fatal("wrong file extent", name)
		}
		found[name] = true
	}
	if len(found) != 3 {
		t.Fatal("missing NoCloud files")
	}
}
func TestUnsafeSeedRejected(t *testing.T) {
	if _, err := ISO(map[string][]byte{"../user-data": {}, "meta-data": {}}); err == nil {
		t.Fatal("accepted unsafe path")
	}
}

func TestISOReadableByIndependentArchiveTool(t *testing.T) {
	tar, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("independent ISO reader unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.iso")
	if err = Write(path, map[string][]byte{"user-data": []byte("#cloud-config\n"), "meta-data": []byte("instance-id: independent\n"), "network-config": []byte("version: 2\n")}); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(tar, "-tf", path).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "does not look like a tar archive") {
			t.Skip("installed tar cannot read ISO volumes")
		}
		t.Fatalf("independent ISO reader: %s: %v", out, err)
	}
	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		if !strings.Contains(string(out), name) {
			t.Fatal("independent reader did not find NoCloud file", name, string(out))
		}
	}
	data, err := exec.Command(tar, "-xOf", path, "user-data").Output()
	if err != nil || string(data) != "#cloud-config\n" {
		t.Fatal("NoCloud content did not round trip through independent reader", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size()%2048 != 0 {
		t.Fatal("invalid ISO size")
	}
}
