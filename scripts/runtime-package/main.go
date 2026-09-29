// runtime-package turns a reviewed llama.cpp directory into a NexusRun runtime package.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/verdictlayer/nexusrun/internal/runtimepkg"
)

func main() {
	var src, out, version, revision, target, libc, server, cli, licenses, provenance string
	flag.StringVar(&src, "source", "", "reviewed runtime directory")
	flag.StringVar(&out, "output", "", "output .runtime.tar.gz")
	flag.StringVar(&version, "version", "", "runtime version")
	flag.StringVar(&revision, "revision", "", "exact upstream revision")
	flag.StringVar(&target, "target", "", "os/arch")
	flag.StringVar(&libc, "libc", "", "supported libc baseline")
	flag.StringVar(&server, "server", "bin/llama-server", "relative llama-server path")
	flag.StringVar(&cli, "cli", "bin/llama-cli", "relative llama-cli path")
	flag.StringVar(&licenses, "licenses", "", "comma-separated SPDX identifiers")
	flag.StringVar(&provenance, "provenance", "", "build provenance URL or description")
	flag.Parse()
	if src == "" || out == "" || version == "" || revision == "" || target == "" || libc == "" || licenses == "" || provenance == "" {
		flag.Usage()
		os.Exit(2)
	}
	parts := strings.Split(target, "/")
	if len(parts) != 2 {
		fatal(fmt.Errorf("target must be os/arch"))
	}
	var paths []string
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular file %s", p)
		}
		rel, _ := filepath.Rel(src, p)
		if filepath.ToSlash(rel) == runtimepkg.MetadataFile {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		fatal(err)
	}
	sort.Strings(paths)
	ph := sha256.New()
	files := make([]runtimepkg.File, 0, len(paths))
	for _, rel := range paths {
		p := filepath.Join(src, filepath.FromSlash(rel))
		f, e := os.Open(p)
		if e != nil {
			fatal(e)
		}
		h := sha256.New()
		n, e := io.Copy(io.MultiWriter(h, ph), f)
		f.Close()
		if e != nil {
			fatal(e)
		}
		info, _ := os.Stat(p)
		files = append(files, runtimepkg.File{Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Mode: uint32(info.Mode().Perm())})
	}
	m := runtimepkg.Metadata{Schema: runtimepkg.Schema, Engine: "llama.cpp", Version: version, UpstreamRevision: revision, OS: parts[0], Arch: parts[1], Accelerator: "cpu", Libc: libc, HostDependencies: []string{"Linux kernel and libc baseline declared by this package"}, Executable: server, CLIExecutable: cli, LibraryPaths: []string{"lib"}, Protocol: "openai-chat/v1", Features: []string{"chat-completions"}, Licenses: strings.Split(licenses, ","), BuildProvenance: provenance, PayloadDigest: hex.EncodeToString(ph.Sum(nil)), Files: files}
	if err = m.Validate(); err != nil {
		fatal(err)
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	o, e := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if e != nil {
		fatal(e)
	}
	gz := gzip.NewWriter(o)
	tw := tar.NewWriter(gz)
	if e = writeBytes(tw, runtimepkg.MetadataFile, mb, 0644); e == nil {
		for _, rel := range paths {
			info, _ := os.Stat(filepath.Join(src, filepath.FromSlash(rel)))
			e = writeFile(tw, src, rel, info)
			if e != nil {
				break
			}
		}
	}
	if ce := tw.Close(); e == nil {
		e = ce
	}
	if ce := gz.Close(); e == nil {
		e = ce
	}
	if ce := o.Close(); e == nil {
		e = ce
	}
	if e != nil {
		fatal(e)
	}
	fmt.Printf("wrote %s\nruntime id: %s\n", out, m.ID())
}
func writeBytes(tw *tar.Writer, name string, b []byte, mode int64) error {
	if e := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b)), Typeflag: tar.TypeReg}); e != nil {
		return e
	}
	_, e := tw.Write(b)
	return e
}
func writeFile(tw *tar.Writer, root, rel string, info os.FileInfo) error {
	if e := tw.WriteHeader(&tar.Header{Name: rel, Mode: int64(info.Mode().Perm()), Size: info.Size(), Typeflag: tar.TypeReg}); e != nil {
		return e
	}
	f, e := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = io.Copy(tw, f)
	return e
}
func fatal(e error) { fmt.Fprintln(os.Stderr, "runtime-package:", e); os.Exit(1) }
