package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/tlsclient"
)

func pairSupervisorCommand(ctx context.Context, serverURL, token string, insecure bool, stderr io.Writer) int {
	if tlsclient.InsecureSkipVerify() {
		fmt.Fprintln(stderr, "WARNING: AIMEBU2_INSECURE_SKIP_VERIFY disables server certificate verification")
	}
	parsed, err := url.Parse(serverURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		fmt.Fprintln(stderr, "pairing requires an http(s) server origin")
		return 2
	}
	if err := tlsclient.CheckTransport(serverURL, insecure); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if insecure && tlsclient.PlainHTTPToARemoteHost(serverURL) {
		fmt.Fprintln(stderr, "WARNING: --insecure: the pairing token and the machine key it returns cross the network over plain http to "+serverURL)
	}
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := refuseV1RootWrite(config.root); err != nil {
		fmt.Fprintln(stderr, reportSupervisorRootError(config.root, err))
		return 1
	}
	path := filepath.Join(config.root, "supervisor", "machine.key")
	if _, err := os.Lstat(path); err == nil {
		fmt.Fprintln(stderr, "remote machine key already exists; refusing to replace it")
		return 1
	} else if !os.IsNotExist(err) {
		fmt.Fprintln(stderr, err)
		return 1
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/api/machine-pairings/redeem", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	client := tlsclient.NewTransportCheckedClient(15*time.Second, insecure)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		fmt.Fprintf(stderr, "pairing failed: HTTP %d\n", resp.StatusCode)
		return 1
	}
	var answer struct {
		MachineID  string `json:"machine_id"`
		MachineKey string `json:"machine_key"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&answer); err != nil || answer.MachineID == "" || answer.MachineKey == "" {
		fmt.Fprintln(stderr, "invalid pairing response")
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeRemoteMachineKey(path, hostMachineCredential{MachineID: answer.MachineID, Key: answer.MachineKey, ServerURL: serverURL}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stderr, "Machine %s paired; waiting for owner approval.\n", answer.MachineID)
	return supervisorCommand(ctx, insecure, stderr)
}

func writeRemoteMachineKey(path string, c hostMachineCredential) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".machine-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(c); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Link(file.Name(), path)
}

func waitForMachineApproval(ctx context.Context, serverURL string, c hostMachineCredential, insecure bool, stderr io.Writer) error {
	client := tlsclient.NewTransportCheckedClient(10*time.Second, insecure)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+"/api/machines/"+url.PathEscape(c.MachineID)+"/pair-status", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Key)
		resp, err := client.Do(req)
		if err == nil {
			var status struct {
				Approved bool `json:"approved"`
			}
			if resp.StatusCode == 200 {
				err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&status)
			} else if resp.StatusCode == 401 {
				err = errors.New("machine key revoked or rejected")
			} else {
				err = fmt.Errorf("machine approval check: HTTP %d", resp.StatusCode)
			}
			resp.Body.Close()
			if err == nil && status.Approved {
				return nil
			}
			if err != nil && resp.StatusCode == 401 {
				return err
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}
