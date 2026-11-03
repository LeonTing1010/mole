package core

import (
	"bytes"
	"encoding"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	bolt "github.com/sagernet/bbolt"
)

// 1.14's FakeIPMetadata() consumes the allocator checkpoint. If the engine is
// killed before the next checkpoint, Start() resets all otherwise committed
// mappings. Repair only offline and only under the qualified engine contract.
// Pin the same bbolt implementation as 1.14.4; never edit its on-disk pages.
func recoverFakeIPCache(configPath string) error {
	var cfg struct {
		DNS struct {
			Servers []struct {
				Type  string `json:"type"`
				Inet4 string `json:"inet4_range"`
				Inet6 string `json:"inet6_range"`
			} `json:"servers"`
		} `json:"dns"`
		Experimental struct {
			Cache struct {
				Enabled bool   `json:"enabled"`
				Store   bool   `json:"store_fakeip"`
				Path    string `json:"path"`
			} `json:"cache_file"`
		} `json:"experimental"`
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	c := cfg.Experimental.Cache
	if !c.Enabled || !c.Store {
		return nil
	}
	var ranges [2]netip.Prefix
	found := false
	for _, s := range cfg.DNS.Servers {
		if s.Type == "fakeip" {
			if found {
				return fmt.Errorf("multiple FakeIP stores are not qualified for cache recovery")
			}
			found = true
			for i, text := range []string{s.Inet4, s.Inet6} {
				if text != "" {
					ranges[i], err = netip.ParsePrefix(text)
					if err != nil || ranges[i].Addr().Is4() != (i == 0) {
						return fmt.Errorf("invalid FakeIP range")
					}
				}
			}
		}
	}
	if !found {
		return nil
	}
	if !filepath.IsAbs(c.Path) {
		return fmt.Errorf("FakeIP recovery requires an absolute cache path")
	}
	st, err := os.Lstat(c.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("FakeIP cache must be a regular private file")
	}
	if st.Size() == 0 {
		return nil
	}
	db, err := bolt.Open(c.Path, 0600, &bolt.Options{Timeout: 250 * time.Millisecond})
	if err != nil {
		return fmt.Errorf("lock FakeIP cache offline: %w", err)
	}
	defer db.Close()
	var checkpoint []byte
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("fakeip_address"))
		if b == nil {
			return nil
		}
		previous := b.Get([]byte("fakeip_metadata"))
		var current [2]netip.Addr
		if len(previous) > 0 {
			p4, p6, a4, a6, err := decodeFakeIPMetadata(previous)
			if err != nil {
				return err
			}
			// An explicit range migration belongs to the engine, not repair.
			if p4 != ranges[0] || p6 != ranges[1] {
				return nil
			}
			current = [2]netip.Addr{a4, a6}
		}
		for i, p := range ranges {
			if p.IsValid() && !current[i].IsValid() {
				current[i] = p.Addr().Next()
			}
		}
		count := 0
		err := b.ForEach(func(k, v []byte) error {
			if bytes.Equal(k, []byte("fakeip_metadata")) {
				return nil
			}
			ip, ok := netip.AddrFromSlice(k)
			if !ok || v == nil || len(v) == 0 || len(v) > 253 || !utf8.Valid(v) {
				return fmt.Errorf("unrecognized FakeIP cache record")
			}
			i := 1
			if ip.Is4() {
				i = 0
			}
			if !ranges[i].Contains(ip) {
				return fmt.Errorf("FakeIP range changed without a checkpoint; cache preserved for explicit migration")
			}
			reverse := tx.Bucket([]byte([]string{"fakeip_domain4", "fakeip_domain6"}[i]))
			if reverse == nil || !bytes.Equal(reverse.Get(v), k) {
				return fmt.Errorf("inconsistent FakeIP reverse mapping; cache preserved")
			}
			if ip.Compare(current[i]) > 0 {
				current[i] = ip
			}
			count++
			return nil
		})
		if err != nil || count == 0 {
			return err
		}
		checkpoint, err = encodeFakeIPMetadata(ranges, current)
		if bytes.Equal(checkpoint, previous) {
			checkpoint = nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if len(checkpoint) == 0 {
		return nil
	}
	// Preserve an exclusive pre-repair snapshot before a single transactional
	// metadata write. The existing forward/reverse mappings stay byte-for-byte.
	prefix := "." + filepath.Base(c.Path) + ".fakeip-recovery-"
	backup, err := os.CreateTemp(filepath.Dir(c.Path), prefix+"*.db")
	if err != nil {
		return err
	}
	name := backup.Name()
	backup.Close()
	if err = db.View(func(tx *bolt.Tx) error { return tx.CopyFile(name, 0600) }); err != nil {
		os.Remove(name)
		return err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("fakeip_address")).Put([]byte("fakeip_metadata"), checkpoint)
	}); err != nil {
		return err
	}
	// A restart storm must not fill the disk with private recovery snapshots.
	entries, _ := os.ReadDir(filepath.Dir(c.Path))
	var backups []os.FileInfo
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".db") && entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				backups = append(backups, info)
			}
		}
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].ModTime().After(backups[j].ModTime()) })
	for i := 3; i < len(backups); i++ {
		if err := os.Remove(filepath.Join(filepath.Dir(c.Path), backups[i].Name())); err != nil {
			log.Printf("FakeIP backup retention: %v", err)
		}
	}
	return nil
}

func encodeFakeIPMetadata(ranges [2]netip.Prefix, current [2]netip.Addr) ([]byte, error) {
	var out bytes.Buffer
	for _, value := range []encoding.BinaryMarshaler{ranges[0], ranges[1], current[0], current[1]} {
		b, err := value.MarshalBinary()
		if err != nil {
			return nil, err
		}
		if err = binary.Write(&out, binary.BigEndian, uint16(len(b))); err != nil {
			return nil, err
		}
		out.Write(b)
	}
	return out.Bytes(), nil
}

func decodeFakeIPMetadata(data []byte) (p4, p6 netip.Prefix, a4, a6 netip.Addr, err error) {
	r := bytes.NewReader(data)
	for _, value := range []encoding.BinaryUnmarshaler{&p4, &p6, &a4, &a6} {
		var n uint16
		if err = binary.Read(r, binary.BigEndian, &n); err != nil {
			return
		}
		b := make([]byte, n)
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		if err = value.UnmarshalBinary(b); err != nil {
			return
		}
	}
	if r.Len() != 0 || (p4.IsValid() && (!p4.Addr().Is4() || !p4.Contains(a4))) || (p6.IsValid() && (p6.Addr().Is4() || !p6.Contains(a6))) {
		err = fmt.Errorf("invalid FakeIP allocator checkpoint")
	}
	return
}
