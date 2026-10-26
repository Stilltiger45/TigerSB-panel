package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// APIBase is the GitHub API; tests replace it.
var APIBase = "https://api.github.com"

// Check is what GitHub says about the newest version.
type Check struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"` // main-<short sha>
	Ref       string    `json:"ref,omitempty"`    // full commit sha to install
	Date      time.Time `json:"date,omitempty"`
	Available bool      `json:"available"`
	Changes   []string  `json:"changes,omitempty"` // newest first
	Checked   time.Time `json:"checked"`
	Error     string    `json:"error,omitempty"`
}

// Version builds from main are named main-<short sha>.
var mainVersion = regexp.MustCompile(`^main-([0-9a-f]{7,40})$`)

// Version returns the version name the installer gives a build of commit sha.
func Version(sha string) string { return "main-" + sha[:7] }

type commit struct {
	SHA     string     `json:"sha"`
	Parents []struct{} `json:"parents"`
	Commit  struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// CheckGitHub asks GitHub for the newest commit on main and what changed
// since the installed one. Errors end up in Check.Error.
func CheckGitHub(ctx context.Context, client *http.Client, current string, now time.Time) Check {
	c := Check{Current: current, Checked: now.UTC()}
	var head commit
	if err := getJSON(ctx, client, "/repos/"+Repo+"/commits/main", &head); err != nil {
		c.Error = err.Error()
		return c
	}
	if !shaRe.MatchString(head.SHA) {
		c.Error = "GitHub вернул странный ответ"
		return c
	}
	c.Ref, c.Latest, c.Date = head.SHA, Version(head.SHA), head.Commit.Committer.Date
	m := mainVersion.FindStringSubmatch(current)
	if m != nil && strings.HasPrefix(head.SHA, m[1]) {
		return c
	}
	c.Available = true
	c.Changes = []string{title(head.Commit.Message)}
	if m == nil {
		return c // an unknown build: nothing to compare against
	}
	var cmp struct {
		Commits []commit `json:"commits"`
	}
	if getJSON(ctx, client, "/repos/"+Repo+"/compare/"+m[1]+"..."+head.SHA, &cmp) != nil {
		return c
	}
	// Changes arrive as merged pull requests; their own commits would repeat
	// them, so with any merge present only merges are listed.
	merges := false
	for _, cm := range cmp.Commits {
		merges = merges || len(cm.Parents) > 1
	}
	c.Changes = nil
	for i := len(cmp.Commits) - 1; i >= 0 && len(c.Changes) < 20; i-- {
		cm := cmp.Commits[i]
		if merges && len(cm.Parents) < 2 {
			continue
		}
		if t := title(cm.Commit.Message); t != "" {
			c.Changes = append(c.Changes, t)
		}
	}
	return c
}

var mergeRe = regexp.MustCompile(`^Merge pull request (#\d+) from `)

// title is the first line of a commit message; for a merged pull request it
// is the pull request's title, which GitHub puts on the third line.
func title(msg string) string {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	first := strings.TrimSpace(lines[0])
	if m := mergeRe.FindStringSubmatch(first); m != nil {
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l != "" {
				return l + " (" + m[1] + ")"
			}
		}
	}
	return first
}

func getJSON(ctx context.Context, client *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "TigerSB-panel")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub недоступен: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub ответил %s", res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(v)
}
