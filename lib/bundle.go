package bundle

// What Hive has no call for: reading the games packed onto the end of this
// executable, putting one on disk, starting it without waiting for it, and
// swapping this executable for a newer one while it is running.
//
// The games are not compiled in. The CI build compiles the arcade first and
// then appends a zip of the game executables to it, followed by a 16-byte
// trailer: the zip's length as a little-endian uint64, then the magic
// "HIVEARC1". An executable runs the same with bytes after its end, and this
// file finds them again by reading the trailer. See tools/bundle.py.
//
// A build with no trailer (`hive run`, or a local `hive build`) falls back to a
// `games/` folder beside the executable or in the working directory, so the
// arcade can be tried locally with executables dropped there by hand.

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const magic = "HIVEARC1"

// Platform is the OS and architecture this arcade was built for, as release
// assets name them: "windows-amd64", "linux-amd64".
func Platform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// Suffix is what an executable's name ends with here.
func Suffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func self() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path, nil
}

// Self is the path of the running executable.
func Self() (string, error) {
	return self()
}

func payload() (*zip.Reader, *os.File, error) {
	path, err := self()
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	size := info.Size()
	if size < 16 {
		f.Close()
		return nil, nil, errors.New("no bundle")
	}
	trailer := make([]byte, 16)
	if _, err := f.ReadAt(trailer, size-16); err != nil {
		f.Close()
		return nil, nil, err
	}
	if string(trailer[8:]) != magic {
		f.Close()
		return nil, nil, errors.New("no bundle")
	}
	length := int64(binary.LittleEndian.Uint64(trailer[:8]))
	if length <= 0 || length > size-16 {
		f.Close()
		return nil, nil, errors.New("corrupt bundle trailer")
	}
	section := io.NewSectionReader(f, size-16-length, length)
	r, err := zip.NewReader(section, length)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return r, f, nil
}

func looseDirs() []string {
	dirs := []string{}
	if path, err := self(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(path), "games"))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, filepath.Join(wd, "games"))
	}
	return dirs
}

// Bundled says whether this executable carries an appended bundle at all.
func Bundled() bool {
	_, f, err := payload()
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Has says whether a game's executable can be found, in the bundle or loose.
func Has(file string) bool {
	if r, f, err := payload(); err == nil {
		defer f.Close()
		for _, entry := range r.File {
			if entry.Name == file {
				return true
			}
		}
	}
	for _, dir := range looseDirs() {
		if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
			return true
		}
	}
	return false
}

// Note is the text stored beside a file in the bundle — the release each game
// was taken from — or "" when there is none.
func Note(file string) string {
	r, f, err := payload()
	if err != nil {
		return ""
	}
	defer f.Close()
	for _, entry := range r.File {
		if entry.Name == file+".tag" {
			rc, err := entry.Open()
			if err != nil {
				return ""
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func home(id string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "hive-arcade", id)
	return dir, os.MkdirAll(dir, 0o755)
}

// Prepare puts a game's executable on disk, in a folder of the game's own that
// survives updates (so whatever a game saves beside itself is kept), and
// answers with its path. It is only rewritten when what the bundle holds has
// changed, so a second launch costs nothing.
func Prepare(file string, id string) (string, error) {
	dir, err := home(id)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, file)
	stampPath := dest + ".stamp"

	var source io.ReadCloser
	var stamp string

	if r, f, err := payload(); err == nil {
		defer f.Close()
		for _, entry := range r.File {
			if entry.Name == file {
				stamp = fmt.Sprintf("%08x-%d", entry.CRC32, entry.UncompressedSize64)
				if old, err := os.ReadFile(stampPath); err == nil && string(old) == stamp {
					if _, err := os.Stat(dest); err == nil {
						return dest, nil
					}
				}
				rc, err := entry.Open()
				if err != nil {
					return "", err
				}
				source = rc
				break
			}
		}
	}

	if source == nil {
		for _, loose := range looseDirs() {
			candidate := filepath.Join(loose, file)
			b, err := os.ReadFile(candidate)
			if err != nil {
				continue
			}
			stamp = fmt.Sprintf("%08x-%d", crc32.ChecksumIEEE(b), len(b))
			if old, err := os.ReadFile(stampPath); err == nil && string(old) == stamp {
				if _, err := os.Stat(dest); err == nil {
					return dest, nil
				}
			}
			source = io.NopCloser(bytes.NewReader(b))
			break
		}
	}

	if source == nil {
		return "", fmt.Errorf("%s is not in this arcade", file)
	}
	defer source.Close()

	temp := dest + ".part"
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, source); err != nil {
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(temp)
		return "", err
	}
	if err := os.Rename(temp, dest); err != nil {
		os.Remove(temp)
		if _, statErr := os.Stat(dest); statErr == nil {
			// The old copy is still running and Windows will not let it be
			// replaced. Launching that one is better than launching nothing.
			return dest, nil
		}
		return "", err
	}
	os.WriteFile(stampPath, []byte(stamp), 0o644)
	return dest, nil
}

// Launch starts a program in its own folder and does not wait for it.
func Launch(path string) error {
	cmd := exec.Command(path)
	cmd.Dir = filepath.Dir(path)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// Download streams url into a file beside the running executable and answers
// with its path. It is here rather than in hive.net because the arcade is tens
// of megabytes, and a whole-body request with a deadline is the wrong tool for
// that on a slow connection.
func Download(url string) (string, error) {
	path, err := self()
	if err != nil {
		return "", err
	}
	dest := path + ".new"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "hive-arcade")
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download answered %d", resp.StatusCode)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	written, err := io.Copy(out, resp.Body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && resp.ContentLength > 0 && written != resp.ContentLength {
		err = fmt.Errorf("download stopped at %d of %d bytes", written, resp.ContentLength)
	}
	if err != nil {
		os.Remove(dest)
		return "", err
	}
	return dest, nil
}

// Install replaces the running executable with the file at downloaded. The
// running one is renamed aside first — Windows allows renaming a running
// executable but not overwriting it — and removed by Cleanup on the next start.
func Install(downloaded string) error {
	path, err := self()
	if err != nil {
		return err
	}
	if err := os.Chmod(downloaded, 0o755); err != nil {
		return err
	}
	old := path + ".old"
	os.Remove(old)
	if err := os.Rename(path, old); err != nil {
		return err
	}
	if err := os.Rename(downloaded, path); err != nil {
		os.Rename(old, path)
		return err
	}
	return nil
}

// Cleanup removes what a previous Install left aside.
func Cleanup() {
	if path, err := self(); err == nil {
		os.Remove(path + ".old")
		os.Remove(path + ".new")
	}
}

// Restart starts this executable again — the new one, after Install.
func Restart() error {
	path, err := self()
	if err != nil {
		return err
	}
	return Launch(path)
}
