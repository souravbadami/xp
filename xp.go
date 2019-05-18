package main

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/pkg/errors"
)

type data struct {
	Devs  map[string]*dev  `json:"devs"`
	Repos map[string]*repo `json:"repos"`
}

func load(r io.Reader) (*data, error) {
	bytes, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, errors.Wrap(err, "read failed")
	}

	var d data
	if err := yaml.Unmarshal(bytes, &d); err != nil {
		return nil, errors.Wrap(err, "unmarshall failed")
	}

	return &d, nil
}

func (d *data) String() string {
	b, err := yaml.Marshal(d)
	if err != nil {
		panic(err)
	}

	return string(b)
}

func (d *data) store(w io.Writer) error {
	if _, err := io.Copy(w, strings.NewReader(d.String())); err != nil {
		return errors.Wrap(err, "store failed")
	}

	return nil
}

type dev struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (d *dev) String() string {
	return d.Name + " <" + d.Email + ">"
}

func (d *data) addDev(id, name, email string) {
	if d.Devs == nil {
		d.Devs = make(map[string]*dev)
	}
	d.Devs[id] = &dev{Name: name, Email: email}
}

func (d *data) lookupDev(id string) *dev {
	if d.Devs == nil {
		return nil
	}
	return d.Devs[id]
}

type repo struct {
	Devs    []string `json:"devs"`
	IssueID string   `json:"issueId"`
}

func (d *data) validateDevs(devIDs []string) error {
	for _, did := range devIDs {
		if d.lookupDev(did) == nil {
			return errors.Errorf("no dev with id %s found", did)
		}
	}
	return nil
}

func (d *data) addRepo(path string, devIDs []string, issueID string) error {
	if d.Repos == nil {
		d.Repos = make(map[string]*repo)
	}

	if err := d.validateDevs(devIDs); err != nil {
		return errors.Wrap(err, "dev ids validation failed")
	}

	d.Repos[path] = &repo{
		Devs:    devIDs,
		IssueID: issueID,
	}

	return nil
}

func initRepo(pathStr string, overwrite bool, xpBinPath string) error {
	gitPath := path.Join(pathStr, ".git")

	if _, err := os.Stat(gitPath); err != nil {
		return errors.Wrapf(err, ".git not found in %s", pathStr)
	}

	if !overwrite {
		for _, hookFile := range hookFiles {
			if _, err := os.Stat(path.Join(gitPath, hookFile)); err == nil {
				// TODO: Check if it is our prepare-commit-msg hook.
				return errors.Errorf("%s is already defined", hookFile)
			}
		}
	}

	hookStr := fmt.Sprintf(hookStrTmpl, xpBinPath)

	for _, hookFile := range hookFiles {
		hookFile = path.Join(gitPath, hookFile)

		f, err := os.OpenFile(hookFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
		if err != nil {
			return errors.Wrapf(err, "create hook file %s failed", hookFile)
		}

		if _, err := f.WriteString(hookStr); err != nil {
			return errors.Wrap(err, "write hook content failed")
		}

		if err := f.Close(); err != nil {
			return errors.Wrap(err, "close hook file failed")
		}
	}

	return nil
}

var hookFiles = []string{
	"hooks/prepare-commit-msg",
	"hooks/commit-msg",
}

var hookStrTmpl = `#!/bin/sh
%s add-info $1
`

func (d *data) lookupRepo(pathStr string) (string, *repo) {
	if d.Repos == nil {
		return "", nil
	}

	r := d.Repos[pathStr]
	if r != nil {
		return pathStr, r
	}

	for k, v := range d.Repos {
		matched, err := path.Match(k+"/*", pathStr)
		if err != nil {
			log.Printf("match failed for %s", pathStr)
			continue
		}
		if matched {
			return k, v
		}
	}

	return "", nil
}

func (d *data) updateRepoDevs(wd string, devIDs []string) error {
	_, repo := d.lookupRepo(wd)
	if repo == nil {
		return errors.Errorf("no repo with path %s found", wd)
	}

	if err := d.validateDevs(devIDs); err != nil {
		return errors.Wrap(err, "dev ids validation failed")
	}

	repo.Devs = devIDs

	return nil
}

const issueIDPrefix = "Issue-id: "

func (d *data) appendInfo(wd, msgFile string) error {
	repoPath, repo := d.lookupRepo(wd)
	if repo == nil {
		return errors.Errorf("no repo with path %s found", wd)
	}

	// GIT_COMMITTER_IDENT can be used to get committer info.
	author, err := gitVar("GIT_AUTHOR_IDENT")
	if err != nil {
		return errors.Wrap(err, "get author info failed")
	}
	authorName, authorEmail := nameEmail(author)

	msg, err := ioutil.ReadFile(msgFile)
	if err != nil {
		return errors.Wrapf(err, "read commit msg from file %s failed", msgFile)
	}

	var (
		msgStr = string(msg)

		devs    = make(map[string]*dev)
		edevs   = existingDevs(msgStr)
		issueID = existingIssueID(msgStr)
	)

	for _, dev := range edevs {
		devs[dev.Email] = dev
	}

	ids, endIdx := firstLineIDs(msgStr)
	if len(ids) != 0 {
		msgStr = msgStr[endIdx:]

		devs = make(map[string]*dev)
		for i, id := range ids {
			dev := d.lookupDev(id)
			if dev != nil {
				devs[dev.Email] = dev
				continue
			}

			if i == 0 && issueIDRegexp.MatchString(id) {
				// We will assume the the first id (if not a dev)
				// is the issue id.
				issueID = id
				continue
			}
			return errors.Errorf("non-existing dev %s provided in the first line", id)
		}
	}

	// We only look at repo devs if both existing and first line devs
	// are not specifying any devs.
	if len(devs) == 0 {
		for _, devID := range repo.Devs {
			dev := d.lookupDev(devID)
			if dev == nil {
				return errors.Errorf("non-existing dev %s marked as working for repo %s", devID, repoPath)
			}

			devs[dev.Email] = dev
		}
	}

	issueIDIdx := strings.Index(msgStr, issueIDPrefix)
	if issueIDIdx != -1 {
		msgStr = msgStr[:issueIDIdx-1]
	} else {
		coAuthorIdx := strings.Index(msgStr, "Co-authored-by:")
		if coAuthorIdx != -1 {
			msgStr = msgStr[:coAuthorIdx-1]
		}
	}

	// The message might have empty space surrounding it.
	// For ex in:
	//
	//   [a,b,c] Hello
	//
	// Once we remove the dev ids from the start of the message,
	// the space before `Hello` would still be there. Same with
	// the `Co-authored-by` lines.
	msgStr = strings.TrimSpace(msgStr)

	f, err := os.OpenFile(msgFile, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return errors.Wrapf(err, "open on commit msg file %s failed", msgFile)
	}
	defer f.Close()

	if _, err := io.Copy(f, strings.NewReader(msgStr)); err != nil {
		return errors.Wrapf(err, "write existing msg back failed")
	}

	fmt.Fprintf(f, "\n\n")

	if issueID != "" {
		if _, err := strconv.Atoi(issueID); err == nil {
			fmt.Fprintf(f, "%s#%s\n\n", issueIDPrefix, issueID)
		} else {
			fmt.Fprintf(f, "%s%s\n\n", issueIDPrefix, issueID)
		}
	}

	// We will write the authors back sorted by their email.
	devEmails := make([]string, 0, len(devs))
	for email := range devs {
		devEmails = append(devEmails, email)
	}
	sort.Strings(devEmails)

	log.Printf("total devs: %v", devs)

	for _, email := range devEmails {
		dev := devs[email]

		if dev.Email == authorEmail && dev.Name == authorName {
			log.Printf("skipping %s (same as author)", dev)
			continue
		}

		fmt.Fprintf(f, "Co-authored-by: %s <%s>\n", dev.Name, dev.Email)
		log.Printf("added %s as author", dev)
	}

	return nil
}

var issueIDRegexp = regexp.MustCompile("#?.*[0-9]+")

func firstLineIDs(msg string) ([]string, int) {
	if len(msg) == 0 {
		return nil, 0
	}

	if msg[0] != '[' {
		return nil, 0
	}

	for i, ch := range msg {
		switch {
		case i > 50:
			return nil, 0

		case ch == '\n':
			return nil, 0

		case ch == ']':
			idsStr := msg[1:i]
			switch {
			case strings.Index(idsStr, ",") != -1:
				return strings.Split(idsStr, ","), i + 1

			case strings.Index(idsStr, "|") != -1:
				return strings.Split(idsStr, "|"), i + 1

			default:
				return []string{idsStr}, i + 1
			}
		}
	}

	return nil, 0
}

var gitVar = func(varStr string) (string, error) {
	output, err := exec.Command("git", "var", varStr).Output()
	if err != nil {
		return "", errors.Wrap(err, "git exec failed")
	}
	return string(output), nil
}

func nameEmail(ident string) (string, string) {
	idx := strings.Index(ident, "<")
	colonIdx := strings.Index(ident, ":")
	nameStart := 0
	if colonIdx != -1 && colonIdx < idx {
		nameStart = colonIdx + 2
	}
	name := ident[nameStart : idx-1]
	email := ident[idx+1 : strings.Index(ident, ">")]
	return name, email
}

func existingIssueID(msg string) string {
	scanner := bufio.NewScanner(strings.NewReader(msg))
	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, issueIDPrefix) {
			continue
		}

		issueID := line[len(issueIDPrefix):]
		if issueIDRegexp.MatchString(issueID) {
			return issueID
		}
	}

	return ""
}

func existingDevs(msg string) []*dev {
	var devs []*dev

	scanner := bufio.NewScanner(strings.NewReader(msg))
	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "Co-authored-by") {
			continue
		}

		name, email := nameEmail(line)
		devs = append(devs, &dev{Name: name, Email: email})
	}

	return devs
}
