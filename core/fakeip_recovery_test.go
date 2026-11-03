package core

import (
	"bytes"
	bolt "github.com/sagernet/bbolt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T, variant string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dbpath := filepath.Join(dir, "cache.db")
	cfg := filepath.Join(dir, "config.json")
	text := `{"dns":{"servers":[{"type":"fakeip","inet4_range":"198.18.0.0/15"}]},"experimental":{"cache_file":{"enabled":true,"store_fakeip":true,"path":"` + dbpath + `"}}}`
	if err := os.WriteFile(cfg, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(dbpath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b, _ := tx.CreateBucket([]byte("fakeip_address"))
		r, _ := tx.CreateBucket([]byte("fakeip_domain4"))
		u, _ := tx.CreateBucket([]byte("selected"))
		u.Put([]byte("proxy"), []byte("REALITY"))
		for _, row := range []struct{ ip, domain string }{{"198.18.0.2", "first.example"}, {"198.18.0.9", "last.example"}} {
			key := netip.MustParseAddr(row.ip).AsSlice()
			b.Put(key, []byte(row.domain))
			r.Put([]byte(row.domain), key)
		}
		switch variant {
		case "stale":
			m, _ := encodeFakeIPMetadata([2]netip.Prefix{netip.MustParsePrefix("198.18.0.0/15"), {}}, [2]netip.Addr{netip.MustParseAddr("198.18.0.2"), {}})
			b.Put([]byte("fakeip_metadata"), m)
		case "mismatch":
			r.Put([]byte("first.example"), []byte{1, 2, 3, 4})
		case "range":
			b.Put([]byte{203, 0, 113, 1}, []byte("outside.example"))
		case "badmeta":
			b.Put([]byte("fakeip_metadata"), []byte{0xff})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	return cfg, dbpath
}

func TestFakeIPRecoveryRestoresAllocatorWithoutLosingMappings(t *testing.T) {
	cfg, path := recoveryFixture(t, "")
	if err := recoverFakeIPCache(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	var metadata []byte
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("fakeip_address"))
		metadata = bytes.Clone(b.Get([]byte("fakeip_metadata")))
		p4, p6, a4, a6, err := decodeFakeIPMetadata(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if p4 != netip.MustParsePrefix("198.18.0.0/15") || p6.IsValid() || a4.String() != "198.18.0.9" || a6.IsValid() {
			t.Fatalf("allocator mismatch %v %v %v %v", p4, p6, a4, a6)
		}
		if string(b.Get([]byte{198, 18, 0, 2})) != "first.example" || string(tx.Bucket([]byte("selected")).Get([]byte("proxy"))) != "REALITY" {
			t.Fatal("unrelated or committed data lost")
		}
		return nil
	})
	db.Close()
	if err := recoverFakeIPCache(cfg); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".cache.db.fakeip-recovery-*.db"))
	if len(backups) != 1 {
		t.Fatalf("want exactly one private recovery backup, got %d", len(backups))
	}
	st, _ := os.Stat(backups[0])
	if st.Mode().Perm() != 0600 {
		t.Fatal("backup exposed DNS metadata")
	}
}

func TestFakeIPRecoveryRejectsAmbiguousOrLiveCacheWithoutMutation(t *testing.T) {
	for _, kind := range []string{"mismatch", "range", "badmeta", "locked"} {
		t.Run(kind, func(t *testing.T) {
			cfg, path := recoveryFixture(t, kind)
			before, _ := os.ReadFile(path)
			if kind == "locked" {
				db, err := bolt.Open(path, 0600, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
			}
			started := time.Now()
			err := recoverFakeIPCache(cfg)
			if err == nil {
				t.Fatal("unsafe cache must be refused")
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("live-cache lock must be bounded")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected recovery changed cache")
			}
		})
	}
}

func TestFakeIPRecoveryAdvancesStaleCheckpoint(t *testing.T) {
	cfg, path := recoveryFixture(t, "stale")
	if err := recoverFakeIPCache(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.View(func(tx *bolt.Tx) error {
		_, _, current, _, err := decodeFakeIPMetadata(tx.Bucket([]byte("fakeip_address")).Get([]byte("fakeip_metadata")))
		if err != nil || current.String() != "198.18.0.9" {
			t.Fatalf("stale allocator can reuse acknowledged mappings: %v %v", current, err)
		}
		return nil
	})
}

func TestFakeIPRecoveryIncludesIPv6Allocator(t *testing.T) {
	cfg, path := recoveryFixture(t, "")
	data, _ := os.ReadFile(cfg)
	data = bytes.Replace(data, []byte(`"inet4_range":"198.18.0.0/15"`), []byte(`"inet4_range":"198.18.0.0/15","inet6_range":"fc00::/18"`), 1)
	os.WriteFile(cfg, data, 0600)
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("fakeip_address"))
		r, _ := tx.CreateBucket([]byte("fakeip_domain6"))
		key := netip.MustParseAddr("fc00::a").AsSlice()
		b.Put(key, []byte("v6.example"))
		return r.Put([]byte("v6.example"), key)
	})
	db.Close()
	if err := recoverFakeIPCache(cfg); err != nil {
		t.Fatal(err)
	}
	db, err = bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.View(func(tx *bolt.Tx) error {
		_, p6, _, current, err := decodeFakeIPMetadata(tx.Bucket([]byte("fakeip_address")).Get([]byte("fakeip_metadata")))
		if err != nil || p6.String() != "fc00::/18" || current.String() != "fc00::a" {
			t.Fatalf("IPv6 allocator %v %v %v", p6, current, err)
		}
		return nil
	})
}

func TestFakeIPRecoveryBoundsSnapshotRetention(t *testing.T) {
	cfg, path := recoveryFixture(t, "")
	for i := 0; i < 5; i++ {
		db, err := bolt.Open(path, 0600, nil)
		if err != nil {
			t.Fatal(err)
		}
		db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("fakeip_address")).Delete([]byte("fakeip_metadata")) })
		db.Close()
		if err := recoverFakeIPCache(cfg); err != nil {
			t.Fatal(err)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".cache.db.fakeip-recovery-*.db"))
	if len(backups) != 3 {
		t.Fatalf("restart storm retained %d snapshots; want 3", len(backups))
	}
}
