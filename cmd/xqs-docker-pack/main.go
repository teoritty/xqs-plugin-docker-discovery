// Command xqs-docker-pack builds an installable .xqsp bundle: the manifest, the binary, the icons,
// and a SHA256SUMS covering all of them.
//
// The checksums file is not optional decoration. The host validates set-equality both ways when it
// is present — a file on disk that SHA256SUMS does not list refuses the plugin, and so does a
// listed file that is missing — which is what makes a tampered bundle fail to install rather than
// install quietly.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	src := flag.String("src", "dist/stage", "directory holding plugin.json, the binary and ui/")
	out := flag.String("out", "dist/xqs-docker.xqsp", "bundle to write")
	flag.Parse()

	if err := run(*src, *out); err != nil {
		log.Fatal(err)
	}
}

func run(src, out string) error {
	files, err := collect(src)
	if err != nil {
		return err
	}
	if _, ok := files["plugin.json"]; !ok {
		return fmt.Errorf("pack: %s has no plugin.json", src)
	}
	if err := writeChecksums(src, files); err != nil {
		return err
	}
	// Re-collected so SHA256SUMS itself is in the archive. It is deliberately not in its own
	// listing: a file cannot contain its own hash.
	files, err = collect(src)
	if err != nil {
		return err
	}
	return writeZip(src, files, out)
}

// collect lists every regular file under src, relative and slash-separated.
func collect(root string) (map[string]struct{}, error) {
	files := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	return files, err
}

// writeChecksums writes SHA256SUMS with LF line endings.
//
// LF regardless of platform: the host normalizes CRLF before hashing the file for a signature, so a
// bundle packed on Windows and one packed on Linux must produce the same bytes or the same source
// tree would sign differently depending on who built it.
func writeChecksums(root string, files map[string]struct{}) error {
	names := make([]string, 0, len(files))
	for name := range files {
		if name == "SHA256SUMS" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var sb strings.Builder
	for _, name := range names {
		sum, err := hashFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		sb.WriteString(sum)
		sb.WriteString("  ")
		sb.WriteString(name)
		sb.WriteString("\n")
	}
	return os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(sb.String()), 0o644)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeZip(root string, files map[string]struct{}, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	archive, err := os.Create(out)
	if err != nil {
		return err
	}
	defer archive.Close()

	zw := zip.NewWriter(archive)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := addFile(zw, root, name); err != nil {
			return err
		}
	}
	return zw.Close()
}

func addFile(zw *zip.Writer, root, name string) error {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		return err
	}
	defer f.Close()

	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}
