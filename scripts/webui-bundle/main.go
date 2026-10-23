// Command webui-bundle packs a built Web UI into a release bundle: the archive
// a server loads with LOOMCYCLE_WEBUI_BUNDLE_URL, or unpacked with
// LOOMCYCLE_WEBUI_DIR.
//
// Usage:
//
//	webui-bundle -dist internal/webui/dist -version 1.2.3 -out bin/webui [-api-min N] [-api-max M]
//
// It writes two files into -out:
//
//	loomcycle-webui-<version>.tar.gz         the bundle
//	loomcycle-webui-<version>.tar.gz.sha256  "<hex>  <archive name>\n" (shasum -a 256 -c)
//
// The archive's top level is the bundle itself: manifest.json first, then
// every directory and file of -dist in sorted order, with no leading
// directory. manifest.json lists every file with its SHA-256 (.gitkeep and a
// stale manifest.json at the root of -dist are left out). Entries are regular
// files and directories only, with a fixed mtime, owner and mode, and the gzip
// header carries no name or time, so the same -dist built with the same Go
// toolchain produces the same bytes.
//
// The API level range defaults to this tree's webui.APILevel, the level the UI
// in web/ is written against. A wider range may be declared, but it must
// include that level: a bundle built from this tree that claimed not to work
// with this tree's API would be wrong.
//
// Before writing anything, the tool unpacks the archive it built into a
// temporary directory and loads it with webui.LoadDir, the server's own
// loader. So every rule a server applies (ui_version characters, path rules,
// size limits, index.html) is applied here by the same code, and an archive a
// server would refuse is never written.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/webui"
)

const manifestName = "manifest.json"

// entryTime is every archive entry's mtime. A fixed value is what makes two
// builds of the same files byte-identical; the server never reads it.
var entryTime = time.Unix(0, 0).UTC()

type options struct {
	dist    string
	version string
	out     string
	apiMin  int
	apiMax  int
}

// result names what was written.
type result struct {
	archivePath string
	sumPath     string
	digest      string
}

func main() {
	var o options
	flag.StringVar(&o.dist, "dist", "", "the built Web UI directory (make build-ui writes internal/webui/dist)")
	flag.StringVar(&o.version, "version", "", "the bundle's ui_version, e.g. 1.2.3 (letters, digits and . _ + -)")
	flag.StringVar(&o.out, "out", "", "the directory to write the archive and its .sha256 into")
	flag.IntVar(&o.apiMin, "api-min", webui.APILevel, "the lowest server UI API level the bundle works with")
	flag.IntVar(&o.apiMax, "api-max", webui.APILevel, "the highest server UI API level the bundle works with")
	flag.Parse()
	if flag.NArg() != 0 || o.dist == "" || o.version == "" || o.out == "" {
		fmt.Fprintln(os.Stderr, "usage: webui-bundle -dist DIR -version VERSION -out DIR [-api-min N] [-api-max M]")
		os.Exit(2)
	}
	res, err := run(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "webui-bundle:", err)
		os.Exit(1)
	}
	fmt.Printf("%s\n%s\nsha256 %s\n", res.archivePath, res.sumPath, res.digest)
}

func run(o options) (result, error) {
	if o.apiMin > webui.APILevel || o.apiMax < webui.APILevel {
		return result{}, fmt.Errorf("api level range [%d, %d] must include %d, the level this tree's Web UI is written against",
			o.apiMin, o.apiMax, webui.APILevel)
	}
	dirs, files, err := collect(o.dist)
	if err != nil {
		return result{}, err
	}
	manifest, err := manifestFor(files, o.version, o.apiMin, o.apiMax)
	if err != nil {
		return result{}, err
	}
	archive, err := pack(manifest, dirs, files)
	if err != nil {
		return result{}, err
	}
	// The version becomes part of a file name below, so it must have passed
	// the loader's ui_version check first.
	if err := selfCheck(archive); err != nil {
		return result{}, err
	}

	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	name := "loomcycle-webui-" + o.version + ".tar.gz"
	res := result{
		archivePath: filepath.Join(o.out, name),
		sumPath:     filepath.Join(o.out, name+".sha256"),
		digest:      digest,
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return result{}, err
	}
	if err := os.WriteFile(res.archivePath, archive, 0o644); err != nil {
		return result{}, err
	}
	if err := os.WriteFile(res.sumPath, []byte(digest+"  "+name+"\n"), 0o644); err != nil {
		return result{}, err
	}
	return res, nil
}

// collect reads every directory and regular file under dist, as slash paths
// relative to it. A symlink or any other non-regular file is refused rather
// than skipped or followed: what a link points at depends on the machine that
// builds, and the server refuses links anyway.
func collect(dist string) (dirs []string, files map[string][]byte, err error) {
	root, err := os.OpenRoot(dist)
	if err != nil {
		return nil, nil, fmt.Errorf("open -dist: %w", err)
	}
	defer root.Close()
	if _, err := root.Lstat("index.html"); err != nil {
		return nil, nil, fmt.Errorf("%s has no index.html; is it the output of make build-ui? (%w)", dist, err)
	}

	files = make(map[string][]byte)
	fsys := root.FS()
	err = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case name == ".":
			return nil
		case d.IsDir():
			dirs = append(dirs, name)
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("%q is not a regular file (%s); a bundle holds only files and directories", name, d.Type())
		case path.Base(name) == ".gitkeep", name == manifestName:
			// .gitkeep keeps internal/webui/dist in git; a root manifest.json
			// is a previous bundle's, and this run writes its own.
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		files[name] = data
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return dirs, files, nil
}

// manifestFor encodes the manifest through webui.Manifest, the type the server
// decodes, so a field cannot be misspelled here. encoding/json sorts map keys,
// so the bytes are fixed for fixed input.
func manifestFor(files map[string][]byte, version string, apiMin, apiMax int) ([]byte, error) {
	m := webui.Manifest{
		UIVersion:   version,
		APILevelMin: apiMin,
		APILevelMax: apiMax,
		Files:       make(map[string]string, len(files)),
	}
	for name, data := range files {
		sum := sha256.Sum256(data)
		m.Files[name] = hex.EncodeToString(sum[:])
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// pack writes the gzipped tar: manifest.json first, so a reader can check the
// manifest before the rest, then directories and files sorted by path. A
// directory "a/" sorts before everything under it, so a parent always comes
// before its children.
func pack(manifest []byte, dirs []string, files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(dirs)+len(files))
	for _, d := range dirs {
		names = append(names, d+"/")
	}
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	// gzip.NewWriterLevel leaves Header.Name, Comment and ModTime empty, so
	// the gzip header holds no name or time.
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)
	if err := writeFile(tw, manifestName, manifest); err != nil {
		return nil, err
	}
	for _, name := range names {
		if name[len(name)-1] == '/' {
			err = tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     name,
				Mode:     0o755,
				ModTime:  entryTime,
			})
		} else {
			err = writeFile(tw, name, files[name])
		}
		if err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		ModTime:  entryTime,
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// selfCheck unpacks archive into a temporary directory and loads it with the
// server's loader. The unpacking only has to handle archives pack wrote; it
// still goes through an os.Root and refuses anything but files and
// directories, so a mistake in pack fails here instead of writing outside the
// directory.
func selfCheck(archive []byte) error {
	dir, err := os.MkdirTemp("", "webui-bundle-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := unpack(archive, dir); err != nil {
		return fmt.Errorf("unpack the built archive: %w", err)
	}
	if _, err := webui.LoadDir(dir); err != nil {
		return fmt.Errorf("the server would refuse this bundle: %w", err)
	}
	return nil
}

func unpack(archive []byte, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(h.Name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := root.WriteFile(h.Name, data, 0o644); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%q: unexpected entry type %q", h.Name, h.Typeflag)
		}
	}
}
