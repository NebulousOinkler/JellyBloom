package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/secrets"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
)

// syncCmd: jellybloom sync [-user NAME] [-password-file FILE] URL_A URL_B
//
// Merges user state between two running JellyBloom instances, in both
// directions: watch state, resume positions, favorites, ratings, and
// playlists. The most recent change wins per item and playlist.
func syncCmd(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	user := fs.String("user", "", "an admin user (admin: true) that exists on both instances")
	pwFile := fs.String("password-file", "", "read the user's password from this file (default: prompt)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: jellybloom sync -user NAME [-password-file FILE] URL_A URL_B")
		fmt.Fprintln(os.Stderr, "Merges watch state and playlists between two JellyBloom instances, both ways;")
		fmt.Fprintln(os.Stderr, "the most recent change wins. Users are matched by name.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || *user == "" {
		fs.Usage()
		return errors.New("need -user and two instance URLs")
	}
	var pw string
	var err error
	if *pwFile != "" {
		if warn := secrets.LoosePermissions(*pwFile); warn != "" {
			fmt.Fprintln(os.Stderr, "warning:", *pwFile+":", warn)
		}
		pw, err = secrets.ReadPasswordFile(*pwFile)
	} else {
		pw, err = prompt("Password for "+*user, true)
	}
	if err != nil {
		return err
	}

	hc := &http.Client{Timeout: 5 * time.Minute}
	a, err := login(hc, fs.Arg(0), *user, pw)
	if err != nil {
		return err
	}
	b, err := login(hc, fs.Arg(1), *user, pw)
	if err != nil {
		return err
	}
	snapA, err := a.export()
	if err != nil {
		return err
	}
	snapB, err := b.export()
	if err != nil {
		return err
	}
	toB, err := b.merge(snapA)
	if err != nil {
		return err
	}
	toA, err := a.merge(snapB)
	if err != nil {
		return err
	}
	for _, x := range []struct {
		to string
		st userdata.MergeStats
	}{{b.base, toB}, {a.base, toA}} {
		fmt.Printf("%s: %d records, %d playlists updated, %d playlists deleted\n", x.to, x.st.Records, x.st.Playlists, x.st.DeletedPlaylists)
	}
	return nil
}

// instance is a logged-in JellyBloom.
type instance struct {
	hc    *http.Client
	base  string
	token string
}

const syncAuth = `MediaBrowser Client="jellybloom-sync", Device="jellybloom", DeviceId="jellybloom-sync", Version="%s"`

func login(hc *http.Client, base, user, pw string) (*instance, error) {
	base = strings.TrimRight(base, "/")
	body, _ := json.Marshal(map[string]string{"Username": user, "Pw": pw})
	req, err := http.NewRequest(http.MethodPost, base+"/Users/AuthenticateByName", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf(syncAuth, upstream.Version))
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: login as %s: HTTP %d", base, user, resp.StatusCode)
	}
	var auth struct{ AccessToken string }
	if err := json.NewDecoder(resp.Body).Decode(&auth); err != nil || auth.AccessToken == "" {
		return nil, fmt.Errorf("%s: login: unexpected answer", base)
	}
	return &instance{hc: hc, base: base, token: auth.AccessToken}, nil
}

func (i *instance) do(method string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, i.base+"/JellyBloom/UserData", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf(syncAuth, upstream.Version)+`, Token="`+i.token+`"`)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := i.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%s: the user must be an admin (admin: true) there", i.base)
		}
		return nil, fmt.Errorf("%s: HTTP %d: %s", i.base, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return resp, nil
}

func (i *instance) export() (*userdata.Snapshot, error) {
	resp, err := i.do(http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var snap userdata.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("%s: reading user state: %w", i.base, err)
	}
	return &snap, nil
}

func (i *instance) merge(snap *userdata.Snapshot) (userdata.MergeStats, error) {
	var st userdata.MergeStats
	body, err := json.Marshal(snap)
	if err != nil {
		return st, err
	}
	resp, err := i.do(http.MethodPost, bytes.NewReader(body))
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	return st, json.NewDecoder(resp.Body).Decode(&st)
}
