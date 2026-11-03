package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

func readServersAt(path string) ([]Server, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var servers []Server
	if err = json.Unmarshal(data, &servers); err != nil {
		return nil, fmt.Errorf("invalid server store %s: %w (preserve the file for recovery)", path, err)
	}
	return servers, nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".mole-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	utils.ChownToMoleDirOwner(f.Name())
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	// Persist the rename before reporting that the instance is recoverable.
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func updateServersAt(parent context.Context, path string, fn func([]Server) ([]Server, error)) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	utils.ChownToMoleDirOwner(f.Name())
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("server store lock: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	servers, err := readServersAt(path)
	if err != nil {
		return err
	}
	servers, err = fn(servers)
	if err != nil {
		return err
	}
	return writePrivateJSON(path, servers)
}

func updateServers(fn func([]Server) ([]Server, error)) error {
	return updateServersAt(context.Background(), utils.ServersPath(), fn)
}

func appendServer(srv Server) error {
	return updateServers(func(servers []Server) ([]Server, error) {
		for _, s := range servers {
			if s.Name == srv.Name || (srv.InstanceID != "" && s.InstanceID == srv.InstanceID) {
				return nil, fmt.Errorf("server name or instance already recorded")
			}
		}
		srv.Active = len(servers) == 0
		return append(servers, srv), nil
	})
}

func changeServer(id, name string, change func(*Server)) error {
	return updateServers(func(servers []Server) ([]Server, error) {
		for i := range servers {
			if (id != "" && servers[i].InstanceID == id) || (id == "" && servers[i].Name == name) {
				change(&servers[i])
				return servers, nil
			}
		}
		return nil, fmt.Errorf("server %q no longer exists locally; refusing to recreate it", name)
	})
}

func removeRecordedServer(id, name string) error {
	return updateServers(func(servers []Server) ([]Server, error) {
		for i, s := range servers {
			if (id != "" && s.InstanceID == id) || (id == "" && s.Name == name) {
				servers = append(servers[:i], servers[i+1:]...)
				if s.Active && len(servers) > 0 {
					servers[0].Active = true
				}
				return servers, nil
			}
		}
		return servers, nil
	})
}
