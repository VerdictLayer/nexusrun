package fleet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

type Enrollment struct {
	HostID     string `json:"hostID"`
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Channel    string `json:"channel"`
	Token      string `json:"token"`
}

type Activation struct {
	Release Release       `json:"release"`
	Passed  int           `json:"passed"`
	Total   int           `json:"total"`
	Suite   *SuiteOutcome `json:"suite,omitempty"`
}

type State struct {
	Active   *Activation `json:"active"`
	Previous *Activation `json:"previous"`
	Pending  *Evaluation `json:"pending,omitempty"`
	// Proof is Pending's signed statement, kept with it so a report retried
	// after a lost connection carries the signature made when it was decided.
	Proof    *Attestation `json:"proof,omitempty"`
	LastCode string       `json:"lastCode,omitempty"`
}

func Root(home string) string { return filepath.Join(home, "fleet") }
func ReleaseHome(home string, release Release) string {
	return filepath.Join(Root(home), "releases", release.ID)
}

func readPrivate(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("fleet state must be a private regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) > 1024*1024 {
		return errors.New("fleet state is too large")
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("invalid fleet state")
	}
	return nil
}

func writePrivate(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".fleet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

var hostTokenPattern = regexp.MustCompile(`^nxr_host_[A-Za-z0-9_-]{43}$`)

func (e Enrollment) validate() error {
	if !idPattern.MatchString(e.HostID) || !ValidName(e.Name) || !ValidChannel(e.Channel) || !hostTokenPattern.MatchString(e.Token) {
		return errors.New("invalid fleet enrollment")
	}
	_, err := NewClient(e.Repository, e.Token)
	return err
}

func LoadEnrollment(home string) (Enrollment, error) {
	var e Enrollment
	err := readPrivate(filepath.Join(Root(home), "enrollment.json"), &e)
	if err != nil {
		return e, err
	}
	return e, e.validate()
}

func SaveEnrollment(home string, e Enrollment) error {
	if err := e.validate(); err != nil {
		return err
	}
	path := filepath.Join(Root(home), "enrollment.json")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("this NexusRun home is already enrolled")
	}
	return writePrivate(path, e)
}

func LoadState(home string) (State, error) {
	var s State
	err := readPrivate(filepath.Join(Root(home), "state.json"), &s)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	return s, err
}

func SaveState(home string, s State) error {
	return writePrivate(filepath.Join(Root(home), "state.json"), s)
}
