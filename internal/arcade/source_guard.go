package arcade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type importSourceStamp struct {
	directory os.FileInfo
	files     map[string]string
}

func stampImportSource(ctx context.Context, path, jar string) (importSourceStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return importSourceStamp{}, err
	}
	stamp := importSourceStamp{directory: info, files: map[string]string{}}
	for _, name := range []string{jar, "server.properties"} {
		if name == "" {
			continue
		}
		sum, err := sourceFileDigest(ctx, path, name)
		if os.IsNotExist(err) && name == "server.properties" {
			continue
		}
		if err != nil {
			return stamp, err
		}
		stamp.files[name] = sum
	}
	return stamp, nil
}
func sourceFileDigest(ctx context.Context, path, name string) (string, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, st, err := openRegularIn(root, name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	const bound = 512 << 20
	if st.Size() > bound {
		return "", fmt.Errorf("selected launch file exceeds 512 MiB verification limit")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(contextReader{ctx: ctx, reader: f}, bound+1))
	if err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if n > bound || st.Size() != after.Size() || !st.ModTime().Equal(after.ModTime()) {
		return "", fmt.Errorf("launch source changed during verification")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
func (stamp importSourceStamp) verify(ctx context.Context, path string, directory bool) error {
	if directory {
		now, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(stamp.directory, now) {
			return fmt.Errorf("import source directory changed; rescan before retrying")
		}
	}
	for name, want := range stamp.files {
		got, err := sourceFileDigest(ctx, path, name)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("import launch source changed (%s); rescan before retrying", name)
		}
	}
	return nil
}

func claimServerDirectory(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0755); err != nil {
		return fmt.Errorf("could not exclusively claim managed server directory: %w", err)
	}
	return nil
}
