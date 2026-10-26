// Package update keeps restore points and updates the panel from GitHub.
//
// An update runs in its own systemd unit (tigersb-update.service), outside the
// panel's cgroup, because the installer restarts the panel halfway through.
// Before it starts, a restore point is taken; if the new version does not come
// up, the point is put back. The approach follows web-panel-proxy (MIT); the
// code is written anew.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// KeepPoints is how many restore points are kept; older ones are deleted.
const KeepPoints = 5

// Layout says where the installed panel lives. Tests point it at a temp dir.
type Layout struct {
	Config string // /etc/tigersb/config.json
	DB     string // /var/lib/tigersb/panel.db
	Bin    string // /usr/local/bin/tigersb
	Units  string // /etc/systemd/system
}

// DefaultLayout is the layout the installer creates.
func DefaultLayout(configPath, dbPath string) Layout {
	return Layout{Config: configPath, DB: dbPath, Bin: "/usr/local/bin/tigersb", Units: "/etc/systemd/system"}
}

func (l Layout) dataDir() string { return filepath.Dir(l.DB) }
func (l Layout) www() string     { return filepath.Join(l.dataDir(), "www") }
func (l Layout) caddy() string   { return filepath.Join(l.dataDir(), "caddy") }
func (l Layout) points() string  { return filepath.Join(l.dataDir(), "restore") }
func (l Layout) jobDir() string  { return filepath.Join(l.dataDir(), "update") }
func (l Layout) dirs() []string  { return []string{l.www(), l.caddy()} }
func (l Layout) files() []string { return []string{l.Config, l.Bin} }
func isUnit(name string) bool {
	return strings.HasPrefix(name, "tigersb") && !strings.Contains(name, "/")
}
func tarName(path string) string { return strings.TrimPrefix(filepath.Clean(path), "/") }
func fromTar(name string) string { return "/" + filepath.Clean(name) }
func (l Layout) unitFiles() []string {
	var out []string
	for _, pat := range []string{"tigersb*.service", "tigersb*.timer"} {
		m, _ := filepath.Glob(filepath.Join(l.Units, pat))
		out = append(out, m...)
	}
	sort.Strings(out)
	return out
}

// Point is a restore point: the panel binary, settings, database, maintenance
// page, Caddy settings with certificates, and the systemd units.
type Point struct {
	ID      string    `json:"id"`
	Version string    `json:"version"`
	Created time.Time `json:"created"`
	Reason  string    `json:"reason"`
	Size    int64     `json:"size"`
}

const manifestName = "tigersb-restore.json"

var idRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(-[0-9])?$`)

// ValidID reports whether id can name a restore point; it keeps paths out.
func ValidID(id string) bool { return idRe.MatchString(id) }

func (l Layout) archive(id string) string { return filepath.Join(l.points(), id+".tar.gz") }

// CreatePoint takes a restore point. st is the open panel database; its copy
// is taken with VACUUM INTO, so the panel keeps running meanwhile.
func CreatePoint(ctx context.Context, l Layout, st *store.Store, version, reason string, now time.Time) (*Point, error) {
	if err := os.MkdirAll(l.points(), 0o700); err != nil {
		return nil, err
	}
	id := now.UTC().Format("20060102-150405")
	for i := 1; fileExists(l.archive(id)); i++ {
		if i > 9 {
			return nil, errors.New("слишком много точек восстановления за одну секунду")
		}
		id = now.UTC().Format("20060102-150405") + fmt.Sprintf("-%d", i)
	}
	p := &Point{ID: id, Version: version, Created: now.UTC(), Reason: reason}

	snap := filepath.Join(l.points(), id+".db.tmp")
	os.Remove(snap)
	if err := st.Snapshot(ctx, snap); err != nil {
		return nil, fmt.Errorf("копия базы: %w", err)
	}
	defer os.Remove(snap)

	tmp := l.archive(id) + ".tmp"
	if err := writePoint(tmp, l, p, snap); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, l.archive(id)); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(l.archive(id)); err == nil {
		p.Size = fi.Size()
	}
	return p, prunePoints(l)
}

func writePoint(path string, l Layout, p *Point, dbCopy string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	manifest, _ := json.Marshal(p)
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(manifest)), ModTime: p.Created, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(manifest); err != nil {
		return err
	}
	if err := addFile(tw, dbCopy, tarName(l.DB)); err != nil {
		return err
	}
	for _, file := range append(l.files(), l.unitFiles()...) {
		if err := addFile(tw, file, tarName(file)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, dir := range l.dirs() {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				fi, err := d.Info()
				if err != nil {
					return err
				}
				return tw.WriteHeader(&tar.Header{Name: tarName(path) + "/", Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime(), Typeflag: tar.TypeDir})
			}
			if !d.Type().IsRegular() {
				return nil // Caddy keeps no links or sockets here; skip anything odd
			}
			return addFile(tw, path, tarName(path))
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Sync()
}

func addFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ListPoints returns restore points, newest first.
func ListPoints(l Layout) ([]Point, error) {
	entries, err := os.ReadDir(l.points())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Point
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".tar.gz")
		if !ok || !ValidID(id) {
			continue
		}
		p, err := readManifest(l.archive(id))
		if err != nil {
			continue
		}
		p.ID = id
		if fi, err := e.Info(); err == nil {
			p.Size = fi.Size()
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func readManifest(path string) (*Point, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		return nil, err
	}
	if h.Name != manifestName {
		return nil, errors.New("это не точка восстановления TigerSB")
	}
	var p Point
	if err := json.NewDecoder(io.LimitReader(tr, 4096)).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// moveFile renames src to dst, copying when they are on different file systems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".tigersb-new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func prunePoints(l Layout) error {
	points, err := ListPoints(l)
	if err != nil {
		return err
	}
	for _, p := range points[min(KeepPoints, len(points)):] {
		if err := os.Remove(l.archive(p.ID)); err != nil {
			return err
		}
	}
	return nil
}

// RestorePoint puts the files of a restore point back. The caller stops the
// panel first and restarts the services afterwards.
func RestorePoint(l Layout, id string) (*Point, error) {
	if !ValidID(id) {
		return nil, errors.New("неверное имя точки восстановления")
	}
	p, err := readManifest(l.archive(id))
	if err != nil {
		return nil, fmt.Errorf("точка восстановления %s не читается: %w", id, err)
	}
	stage, err := os.MkdirTemp(l.dataDir(), ".restore-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if err := extract(l, l.archive(id), stage); err != nil {
		return nil, err
	}
	staged := func(path string) string { return filepath.Join(stage, tarName(path)) }
	if !fileExists(staged(l.DB)) || !fileExists(staged(l.Config)) || !fileExists(staged(l.Bin)) {
		return nil, errors.New("в точке восстановления нет базы, настроек или программы панели")
	}

	// Files first: the database (without the old WAL), settings, binary.
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Remove(l.DB + suffix)
	}
	for _, file := range []string{l.DB, l.Config, l.Bin} {
		if err := moveFile(staged(file), file); err != nil {
			return nil, err
		}
	}
	for _, dir := range l.dirs() {
		old := dir + ".old"
		os.RemoveAll(old)
		if err := os.Rename(dir, old); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if fileExists(staged(dir)) {
			if err := os.Rename(staged(dir), dir); err != nil {
				return nil, err
			}
		}
		os.RemoveAll(old)
	}
	// Units: exactly the set from the point, so a newer unit the old binary
	// cannot run does not stay behind.
	for _, u := range l.unitFiles() {
		if !fileExists(staged(u)) {
			if err := os.Remove(u); err != nil {
				return nil, err
			}
		}
	}
	units, _ := os.ReadDir(staged(l.Units))
	for _, u := range units {
		if !isUnit(u.Name()) {
			continue
		}
		if err := moveFile(filepath.Join(staged(l.Units), u.Name()), filepath.Join(l.Units, u.Name())); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// extract unpacks only the paths a restore point may hold.
func extract(l Layout, archive, stage string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Name == manifestName {
			continue
		}
		target := fromTar(h.Name)
		if !l.allowed(target) {
			return fmt.Errorf("лишний путь в точке восстановления: %s", h.Name)
		}
		dst := filepath.Join(stage, tarName(target))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, os.FileMode(h.Mode).Perm()|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode).Perm())
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("неподдерживаемый тип файла в точке восстановления: %s", h.Name)
		}
	}
}

func (l Layout) allowed(path string) bool {
	switch path {
	case filepath.Clean(l.DB), filepath.Clean(l.Config), filepath.Clean(l.Bin):
		return true
	}
	if filepath.Dir(path) == filepath.Clean(l.Units) && isUnit(filepath.Base(path)) {
		return true
	}
	for _, dir := range l.dirs() {
		if path == dir || strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}
