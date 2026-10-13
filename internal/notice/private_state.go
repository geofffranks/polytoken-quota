package notice

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// privateSessionQuota pins the same owned, private directory used by diagnostics
// and state transitions. Rooted operations remain confined if paths are swapped.
func privateSessionQuota(path, id string) *os.Root {
	if !safeSessionID(id) || !filepath.IsAbs(path) {
		return nil
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil
	}
	defer root.Close()
	st, err := root.Stat(".")
	if err != nil || !ownedDirectory(st) || st.Mode().Perm()&0022 != 0 {
		return nil
	}
	st, err = root.Lstat(id)
	if err != nil || !ownedDirectory(st) || st.Mode().Perm()&0022 != 0 {
		return nil
	}
	session, err := root.OpenRoot(id)
	if err != nil {
		return nil
	}
	defer session.Close()
	opened, err := session.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		return nil
	}
	if err := session.Mkdir("polytoken-quota", 0700); err != nil && !os.IsExist(err) {
		return nil
	}
	st, err = session.Lstat("polytoken-quota")
	if err != nil || !ownedDirectory(st) || st.Mode().Perm() != 0700 {
		return nil
	}
	quota, err := session.OpenRoot("polytoken-quota")
	if err != nil {
		return nil
	}
	opened, err = quota.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		quota.Close()
		return nil
	}
	return quota
}

func privateStateInfo(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && st.Mode().IsRegular() && st.Mode().Perm() == 0600 && s.Nlink == 1 && s.Uid == uint32(os.Getuid())
}

func rootedStateBytes(root *os.Root) ([]byte, string) {
	st, err := root.Lstat("state.json")
	if os.IsNotExist(err) {
		return nil, "missing"
	}
	if err != nil {
		return nil, "read_failed"
	}
	if !privateStateInfo(st) {
		return nil, "unsafe_state"
	}
	f, err := root.OpenFile("state.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "read_failed"
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !privateStateInfo(opened) || !os.SameFile(st, opened) {
		return nil, "unsafe_state"
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, "read_failed"
	}
	return b, "available"
}

// writeState only writes via the quota root retained under state.lock. The
// temporary file is exclusively created and renamed within that same root.
func (d HookDeps) writeState(st hookState) error {
	root := d.stateRoot
	if root == nil {
		return os.ErrInvalid
	}
	if st, err := root.Lstat("state.json"); err == nil {
		if !privateStateInfo(st) {
			return os.ErrPermission
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := ".state-" + hex.EncodeToString(nonce[:])
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(name, "state.json")
}
