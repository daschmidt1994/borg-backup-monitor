// Package demo provides the clearly marked demo mode: an example
// configuration, example history and a simulated borg – no real repository
// is touched.
package demo

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/borg"
	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

const yaml = `
listen: "127.0.0.1:8080"
restore:
  enabled: true
  max_bytes: 50MB
  max_files: 500
  timeout: 2m
defaults:
  interval: 1d
  tolerance: 6h
  restore_test_max_age: 30d
hosts:
  - name: nas
    jobs:
      - name: system
        description: /etc, /home, Docker-Volumes
        ping_token: demo-nas-system-0001
        repositories:
          - name: storagebox
            location: ssh://u123456@u123456.your-storagebox.de:23/./borg/nas
          - name: usb-platte
            location: /mnt/usb-backup/borg/nas
      - name: fotos
        description: Fotoarchiv, wöchentlich
        ping_token: demo-nas-fotos-0002
        interval: 7d
        tolerance: 1d
        repositories:
          - name: storagebox-fotos
            location: ssh://u123456@u123456.your-storagebox.de:23/./borg/fotos
  - name: webserver
    jobs:
      - name: www-und-datenbank
        description: /var/www und MariaDB-Dump
        ping_token: demo-web-www-0003
        interval: 12h
        tolerance: 2h
        repositories:
          - name: hetzner
            location: ssh://borg@backup.example.net/./web
          - name: offsite
            location: ssh://borg@offsite.example.org/./web
  - name: laptop
    jobs:
      - name: home
        description: Benutzerdaten
        ping_token: demo-laptop-home-0004
        repositories:
          - name: nas-repo
            location: ssh://borg@nas.local/./laptop
  - name: mailserver
    jobs:
      - name: mail
        ping_token: demo-mailserver-mail-0005
        repositories:
          - name: borgbase
            location: ssh://x1y2z3@x1y2z3.repo.borgbase.com/./repo
  - name: db-server
    jobs:
      - name: postgres
        description: ohne borgmatic-Meldung eingerichtet
        repositories:
          - name: storagebox-db
            location: ssh://u123456@u123456.your-storagebox.de:23/./borg/db
  - name: raspberrypi
    jobs:
      - name: config
        description: neu angelegt
        ping_token: demo-raspberrypi-cfg-0006
        repositories:
          - name: lokal
            location: /srv/borg/pi
            query: false
`

// Config is the demo configuration.
func Config(dataDir string) *config.Config {
	c, err := config.Parse([]byte(yaml))
	if err != nil {
		panic(err)
	}
	c.DataDir = dataDir
	c.Restore.BaseDir = filepath.Join(dataDir, "restore-tests")
	return c
}

type repoData struct {
	archives []store.Archive
	sizes    *store.Sizes
	err      *borg.Error
}

// Backend simulates borg for the demo.
type Backend struct {
	repos map[string]*repoData
}

func (b *Backend) Detect(context.Context) *borg.Info {
	return &borg.Info{Version: "1.4.0 (Demo)", Major: 1, Minor: 4, Supported: true, ListCmd: []string{"list"}, BypassLock: true, DetectedAt: time.Now()}
}

func (b *Backend) List(_ context.Context, repo *config.Repository) (*store.RepoSnapshot, *borg.Error) {
	time.Sleep(150 * time.Millisecond)
	snap := &store.RepoSnapshot{RepoID: repo.ID, CheckedAt: time.Now(), BorgVersion: "1.4.0 (Demo)"}
	d := b.repos[repo.ID]
	if d == nil {
		snap.OK = true
		return snap, nil
	}
	if d.err != nil {
		return snap, d.err
	}
	snap.SetArchives(append([]store.Archive(nil), d.archives...))
	snap.OK, snap.Encryption = true, "repokey-blake2"
	return snap, nil
}

func (b *Backend) Sizes(_ context.Context, repo *config.Repository) (*store.Sizes, *borg.Error) {
	if d := b.repos[repo.ID]; d != nil && d.sizes != nil {
		s := *d.sizes
		s.At = time.Now()
		return &s, nil
	}
	return nil, &borg.Error{Finding: store.Finding{Summary: "keine Größen"}}
}

var demoFiles = []borg.Item{
	{Path: "etc", Type: "dir"},
	{Path: "etc/hostname", Type: "file", Size: 4},
	{Path: "etc/hosts", Type: "file", Size: 221},
	{Path: "etc/fstab", Type: "file", Size: 812},
	{Path: "etc/borgmatic", Type: "dir"},
	{Path: "etc/borgmatic/config.yaml", Type: "file", Size: 1934},
	{Path: "home", Type: "dir"},
	{Path: "home/anna", Type: "dir"},
	{Path: "home/anna/Dokumente", Type: "dir"},
	{Path: "home/anna/Dokumente/Steuer-2025.pdf", Type: "file", Size: 482113},
	{Path: "home/anna/Dokumente/Vertrag.odt", Type: "file", Size: 38211},
	{Path: "home/anna/Dokumente/Notizen.txt", Type: "file", Size: 2048},
	{Path: "home/anna/.bashrc", Type: "file", Size: 3771},
	{Path: "home/anna/aktuell", Type: "symlink", LinkTarget: "Dokumente"},
	{Path: "var", Type: "dir"},
	{Path: "var/www", Type: "dir"},
	{Path: "var/www/index.html", Type: "file", Size: 5120},
	{Path: "var/www/bilder/logo.png", Type: "file", Size: 18842},
	{Path: "var/backups/mariadb-dump.sql.gz", Type: "file", Size: 7340032},
}

func (b *Backend) Files(_ context.Context, repo *config.Repository, archive, prefix string, max int) ([]borg.Item, bool, *borg.Error) {
	if d := b.repos[repo.ID]; d != nil && d.err != nil {
		return nil, false, d.err
	}
	var out []borg.Item
	for _, it := range demoFiles {
		if prefix == "" || it.Path == prefix || strings.HasPrefix(it.Path, strings.TrimSuffix(prefix, "/")+"/") {
			out = append(out, it)
			if len(out) >= max {
				return out, true, nil
			}
		}
	}
	return out, false, nil
}

func content(path string, size int64) []byte {
	seed := sha256.Sum256([]byte(path))
	b := make([]byte, size)
	for i := range b {
		b[i] = seed[i%len(seed)] ^ byte(i)
	}
	return b
}

func (b *Backend) Extract(ctx context.Context, repo *config.Repository, spec borg.ExtractSpec) (string, int, error) {
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return "", -1, ctx.Err()
	}
	for _, it := range demoFiles {
		match := false
		for _, p := range spec.Paths {
			if it.Path == p || strings.HasPrefix(it.Path, p+"/") || strings.HasPrefix(p, it.Path+"/") {
				match = true
			}
		}
		if !match {
			continue
		}
		dst := filepath.Join(spec.Dir, filepath.FromSlash(it.Path))
		switch it.Type {
		case "dir":
			_ = os.MkdirAll(dst, 0o755)
		case "file":
			_ = os.MkdirAll(filepath.Dir(dst), 0o755)
			if err := os.WriteFile(dst, content(it.Path, it.Size), 0o644); err != nil {
				return err.Error(), 2, nil
			}
		case "symlink":
			_ = os.MkdirAll(filepath.Dir(dst), 0o755)
			_ = os.Symlink(it.LinkTarget, dst)
		}
	}
	return "", 0, nil
}

// Seed fills the store with a believable history and returns the backend.
func Seed(c *config.Config, st *store.Store, now time.Time) *Backend {
	b := &Backend{repos: map[string]*repoData{}}
	repo := func(host, job, name string) (*config.Job, *config.Repository) {
		for _, j := range c.Jobs() {
			if j.Host == host && j.Job.Name == job {
				for i := range j.Job.Repositories {
					if j.Job.Repositories[i].Name == name {
						return j.Job, &j.Job.Repositories[i]
					}
				}
			}
		}
		panic(host + "/" + job + "/" + name)
	}
	at := func(daysAgo float64, hour, min int) time.Time {
		d := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
		return d.Add(-time.Duration(daysAgo * float64(24*time.Hour)))
	}
	var runs = map[string][]*store.Run{}
	var tests []*store.RestoreTest
	addRun := func(job *config.Job, start time.Time, dur time.Duration, res store.Result, code int, log string) {
		end := start.Add(dur)
		r := &store.Run{ID: fmt.Sprintf("demo-%s-%d", job.ID, start.Unix()), JobID: job.ID, StartedAt: &start, Result: res,
			Source: "healthchecks-ping", ReceivedAt: end}
		if res != store.Running {
			r.FinishedAt = &end
			r.ExitCode = &code
			r.Source = "wrapper"
			r.AppendLog(log)
			r.Findings, _, _, r.Stats = borg.AnalyzeLog(log)
			if res != store.Success {
				_, txt := borg.ClassifyExit(code)
				r.Findings = append(r.Findings, store.Finding{Level: map[store.Result]string{store.Warning: "warning", store.Failure: "error"}[res], Summary: txt})
			}
		} else {
			r.ReceivedAt = start
		}
		runs[job.ID] = append(runs[job.ID], r)
	}
	okLog := func(name string, files int, orig, dedup string) string {
		return fmt.Sprintf(`INFO /etc/borgmatic/config.yaml: Running actions for repository
INFO storagebox: Creating archive
Archive name: %s
Number of files: %d
                       Original size      Compressed size    Deduplicated size
This archive:                %s              8.12 GB            %s
All archives:              402.17 GB            301.55 GB             98.20 GB
terminating with success status, rc 0
INFO summary:
INFO /etc/borgmatic/config.yaml: Successfully ran configuration file`, name, files, orig, dedup)
	}
	archives := func(job *config.Job, count int, every time.Duration, last time.Time) []store.Archive {
		var out []store.Archive
		for i := 0; i < count; i++ {
			t := last.Add(-time.Duration(i) * every)
			e := t.Add(7 * time.Minute)
			out = append(out, store.Archive{Name: fmt.Sprintf("%s-%s", job.Name, t.Format("2006-01-02T15:04:05")), Start: t, End: &e})
		}
		return out
	}

	// nas/system: daily, everything fine; restore test passed 5 days ago
	sys, sb := repo("nas", "system", "storagebox")
	_, usb := repo("nas", "system", "usb-platte")
	for d := 29; d >= 0; d-- {
		s := at(float64(d), 2, 30)
		if s.After(now) {
			continue
		}
		addRun(sys, s, 12*time.Minute+time.Duration(d%5)*time.Minute, store.Success, 0, okLog("nas-"+s.Format("2006-01-02T02:30:00"), 184230+d, "10.84 GB", "214.6 MB"))
	}
	lastSys := *runs[sys.ID][len(runs[sys.ID])-1].StartedAt
	b.repos[sb.ID] = &repoData{archives: archives(sys, 63, 24*time.Hour, lastSys.Add(time.Minute)),
		sizes: &store.Sizes{Source: "borg info", Original: 10840000000, Compressed: 8120000000, Deduplicated: 214600000, AllOriginal: 402170000000, AllDeduplicated: 98200000000}}
	b.repos[usb.ID] = &repoData{archives: archives(sys, 30, 24*time.Hour, lastSys.Add(6*time.Minute))}
	fin := at(5, 10, 14)
	at5 := at(5, 10, 12)
	tests = append(tests, &store.RestoreTest{ID: "demo-rt-1", RepoID: sb.ID, JobID: sys.ID, Archive: "nas-" + at(5, 2, 30).Format("2006-01-02T02:30:00"),
		Paths: []string{"etc/hostname", "home/anna/Dokumente"}, StartedAt: at5, FinishedAt: &fin, StartedBy: "demo", State: "passed",
		Target: "(entfernt)", Removed: true, Bytes: 522376, FileCount: 6, Verified: 4, Limits: "max. 50 MiB, 500 Einträge, 2 Min. Laufzeit",
		Files: []store.RestoreFile{{Path: "etc/hostname", Type: "file", Size: 4, Restored: true, Reference: "match", ReferenceBy: "Prüfsummen-Datei"},
			{Path: "home/anna/Dokumente/Steuer-2025.pdf", Type: "file", Size: 482113, Restored: true, Reference: "match", ReferenceBy: "Prüfsummen-Datei"}},
		Findings: []store.Finding{{Level: "info", Summary: "Stichprobe: Ein bestandener Test zeigt, dass genau diese Dateien aus genau diesem Archiv wiederhergestellt werden konnten. Er garantiert nicht, dass alle Daten wiederherstellbar sind."}}})

	// nas/fotos: weekly, running right now; restore test 45 days ago (stale)
	fotos, sbf := repo("nas", "fotos", "storagebox-fotos")
	for w := 8; w >= 1; w-- {
		s := at(float64(w*7), 3, 0)
		addRun(fotos, s, 2*time.Hour+time.Duration(w)*4*time.Minute, store.Success, 0, okLog("fotos-"+s.Format("2006-01-02"), 912000+w, "1.21 TB", "3.2 GB"))
	}
	addRun(fotos, now.Add(-42*time.Minute), 0, store.Running, 0, "")
	b.repos[sbf.ID] = &repoData{archives: archives(fotos, 26, 7*24*time.Hour, at(7, 3, 1))}
	f45 := at(45, 20, 3)
	tests = append(tests, &store.RestoreTest{ID: "demo-rt-2", RepoID: sbf.ID, JobID: fotos.ID, Archive: "fotos-" + at(46, 3, 0).Format("2006-01-02"),
		Paths: []string{"home/anna/Bilder/2024/urlaub-001.jpg"}, StartedAt: at(45, 20, 1), FinishedAt: &f45, StartedBy: "demo", State: "passed",
		Removed: true, Bytes: 4813221, FileCount: 1, Verified: 1, Limits: "max. 50 MiB"})

	// webserver: twice a day, last run with warnings; offsite repo unreachable
	www, hz := repo("webserver", "www-und-datenbank", "hetzner")
	_, off := repo("webserver", "www-und-datenbank", "offsite")
	for h := 28; h >= 0; h-- {
		s := at(float64(h)/2, 1, 15)
		if h%2 == 1 {
			s = s.Add(12 * time.Hour)
		}
		if s.After(now.Add(-time.Hour)) {
			continue
		}
		res, code, log := store.Success, 0, okLog("www-"+s.Format("2006-01-02T15:04"), 41200, "3.91 GB", "88.1 MB")
		if h == 0 || h == 1 {
			res, code = store.Warning, 1
			log = "WARNING /var/lib/mysql/ibdata1: file changed while we backed it up\n" + log + "\nterminating with warning status, rc 1"
		}
		addRun(www, s, 6*time.Minute, res, code, log)
	}
	lastWWW := *runs[www.ID][len(runs[www.ID])-1].StartedAt
	b.repos[hz.ID] = &repoData{archives: archives(www, 120, 12*time.Hour, lastWWW.Add(time.Minute))}
	b.repos[off.ID] = &repoData{archives: archives(www, 50, 12*time.Hour, lastWWW.Add(-36*time.Hour)),
		err: &borg.Error{Code: 2, Finding: borg.Explain("ConnectionClosed", "Remote: ssh: connect to host offsite.example.org port 22: Connection timed out\nConnection closed by remote host. Is borg working on the server?")}}
	f20 := at(20, 9, 41)
	tests = append(tests, &store.RestoreTest{ID: "demo-rt-3", RepoID: hz.ID, JobID: www.ID, Archive: "www-" + at(20, 1, 15).Format("2006-01-02T15:04"),
		Paths: []string{"var/www/index.html"}, StartedAt: at(20, 9, 40), FinishedAt: &f20, StartedBy: "demo", State: "passed-unverified",
		Removed: true, Bytes: 5120, FileCount: 1, Limits: "max. 50 MiB",
		Findings: []store.Finding{{Level: "warning", Summary: "Inhalt nicht gegen eine unabhängige Referenz geprüft", Hint: "reference_checksums (sha256sum-Datei) oder compare_root für den Job eintragen."}}})

	// laptop: daily expected, last success 4 days ago (overdue)
	lap, nasr := repo("laptop", "home", "nas-repo")
	for d := 20; d >= 4; d-- {
		if d%3 == 2 {
			continue // laptop was off
		}
		s := at(float64(d), 19, 5)
		addRun(lap, s, 25*time.Minute, store.Success, 0, okLog("laptop-"+s.Format("2006-01-02"), 98000, "61.2 GB", "502 MB"))
	}
	b.repos[nasr.ID] = &repoData{archives: archives(lap, 40, 24*time.Hour, at(4, 19, 6))}
	f12 := at(12, 18, 0)
	tests = append(tests, &store.RestoreTest{ID: "demo-rt-4", RepoID: nasr.ID, JobID: lap.ID, Archive: "laptop-" + at(13, 19, 5).Format("2006-01-02"),
		Paths: []string{"home/anna/Dokumente/Vertrag.odt"}, StartedAt: at(12, 17, 59), FinishedAt: &f12, StartedBy: "demo", State: "failed",
		Removed: true, Limits: "max. 50 MiB",
		Findings: []store.Finding{{Level: "error", Summary: "1 Abweichungen bei Größe oder Prüfsumme"},
			{Level: "error", Summary: "Integritätsfehler im Repository", Hint: "Auf dem Backup-Host „borg check“ ausführen und Ursache (Speicher/Datenträger) klären.", Detail: "Data integrity error: Segment entry checksum mismatch [segment 1832, offset 3101]"}}})

	// mail: last two runs failed (no space left)
	mail, bb := repo("mailserver", "mail", "borgbase")
	for d := 14; d >= 0; d-- {
		s := at(float64(d), 4, 0)
		if s.After(now) {
			continue
		}
		if d <= 1 {
			addRun(mail, s, 3*time.Minute, store.Failure, 2, "INFO borgbase: Creating archive\nERROR Remote: Insufficient free space to complete transaction (required: 1.02 GB, available: 211.40 MB).\nterminating with error status, rc 2\nCRITICAL Error running configuration file /etc/borgmatic/config.yaml\nCommand 'borg create' returned non-zero exit status 2.")
		} else {
			addRun(mail, s, 4*time.Minute, store.Success, 0, okLog("mail-"+s.Format("2006-01-02"), 210331, "18.4 GB", "41.0 MB"))
		}
	}
	b.repos[bb.ID] = &repoData{archives: archives(mail, 30, 24*time.Hour, at(2, 4, 1))}

	// db-server: no pings configured – only the archive is known
	_, dbr := repo("db-server", "postgres", "storagebox-db")
	b.repos[dbr.ID] = &repoData{archives: archives(&config.Job{Name: "postgres"}, 90, 24*time.Hour, at(0.5, 1, 0))}

	// raspberrypi: nothing known yet (no report, repository not queried)

	_ = st.Update(func(s *store.State) {
		s.Runs = runs
		s.RestoreTests = tests
	})
	// first query right away, so the dashboard has repository data
	ctx := context.Background()
	for _, j := range c.Jobs() {
		for i := range j.Job.Repositories {
			r := &j.Job.Repositories[i]
			if !r.Queried() {
				continue
			}
			snap, berr := b.List(ctx, r)
			if berr != nil {
				f := berr.Finding
				snap.Error = &f
				if d := b.repos[r.ID]; d != nil && len(d.archives) > 0 {
					snap.SetArchives(append([]store.Archive(nil), d.archives...))
					lg := now.Add(-37 * time.Hour)
					snap.LastGoodAt = &lg
				}
			} else {
				t := snap.CheckedAt
				snap.LastGoodAt = &t
				if d := b.repos[r.ID]; d != nil && d.sizes != nil {
					sz := *d.sizes
					sz.At = now.Add(-2 * time.Hour)
					snap.Sizes = &sz
				}
			}
			_ = st.Update(func(s *store.State) { s.Repos[r.ID] = snap })
		}
	}
	return b
}
