package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
)

func main() {
	if err := observe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// observe watches atomic status-file replacements inside the disposable guest.
// API polling cannot reliably capture the brief starting state.
func observe() error {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, "/run/incus-gh-runner", syscall.IN_MOVED_TO); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, `{"observer":"ready"}`); err != nil {
		return err
	}
	return observeUpdates(fd)
}

func observeUpdates(fd int) error {
	buffer := make([]byte, os.Getpagesize())
	previous := ""
	for {
		if _, err := syscall.Read(fd, buffer); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		data, err := os.ReadFile("/run/incus-gh-runner/status.json")
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		var status struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(data, &status); err != nil {
			return err
		}
		if status.State != previous {
			if _, err := os.Stdout.Write(data); err != nil {
				return err
			}
			previous = status.State
		}
		switch status.State {
		case "exited", "failed":
			return nil
		}
	}
}
