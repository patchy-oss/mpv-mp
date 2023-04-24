package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const programDirPath = "/tmp/mpv-mp"

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	err := os.MkdirAll(programDirPath, 0755)
	if err != nil {
		die("%v: couldn't create dir %q: %v\n", os.Args[0], programDirPath, err)
	}
	pidPath := filepath.Join(programDirPath, "pid")
	ipcPath := filepath.Join(programDirPath, "ipc")

	if !mpvIsRunning(pidPath) {
		mpvStart(pidPath, ipcPath)
	}
	ipc := mpvConnectIPC(ipcPath)
	defer ipc.Close()

	command := os.Args[1]
	switch command {
	case "play":
		playCmd(ipc, os.Args[2:]...)
	case "add":
		addCmd(ipc, appendACM, os.Args[2:]...)
	case "playlist":
		playlistCmd(ipc, os.Args[2:]...)
	case "pause":
		pauseCmd(ipc)
	case "next":
		nextCmd(ipc)
	case "prev":
		prevCmd(ipc)
	case "loop":
		loopCmd(ipc)
	case "kill":
		killCmd(pidPath)
	case "-":
		customCmd(ipc, os.Args[2:]...)
	default:
		usage()
	}

	os.Exit(0)
}

// mpvIsRunning checks if a background mpv instance already running
func mpvIsRunning(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		die("%v: couldn't read file %q: %v\n", os.Args[0], pidPath, err)
	}

	// if we read the pid means mpv is already running (probably!) so we don't need to
	// run a new process
	running := err == nil
	if running {
		procCommPath := filepath.Join("/proc", string(data), "comm")
		procName, err := os.ReadFile(procCommPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			die("%v: couldn't read file %q: %v\n", os.Args[0], procCommPath, err)
		}

		if errors.Is(err, os.ErrNotExist) || !strings.Contains(string(procName), "mpv") {
			running = false
		}
	}

	return running
}

// mpsStart starts new background mpv instance and writes its pid and ipc file in programDirPath
func mpvStart(pidPath, ipcPath string) {
	cmd := exec.Command(
		"mpv",
		"--no-video",
		"--no-terminal",
		"--idle",
		"--loop-playlist",
		fmt.Sprintf("--input-ipc-server=%s", ipcPath),
	)
	err := cmd.Start()
	check(err)

	data := []byte(strconv.Itoa(cmd.Process.Pid))
	err = os.WriteFile(pidPath, data, 0644)
	check(err)
}

// mpvConnectIPC creates unix socket to mpv IPC
func mpvConnectIPC(ipcPath string) net.Conn {
	const maxCheckTimes = 10
	i := 0
	_, err := os.Stat(ipcPath)
	for ; err != nil && i < maxCheckTimes; _, err = os.Stat(ipcPath) {
		if !errors.Is(err, os.ErrNotExist) {
			die("%v: couldn't stat file %q: %v\n", os.Args[0], ipcPath, err)
		}
		time.Sleep(100 * time.Millisecond)
		i++
	}
	if i >= maxCheckTimes || err != nil {
		die("%v: couldn't get mpv ipc %q: %v\n", os.Args[0], ipcPath, err)
	}
	ipc, err := net.Dial("unix", ipcPath)
	check(err)

	return ipc
}

// playCmd plays the current playlist from the beginning or replaces it with args
func playCmd(ipc net.Conn, args ...string) {
	if len(args) == 0 {
		playlistCmd(ipc, "0")
	} else {
		addCmd(ipc, replaceACM, args...)
	}

	paused := mpvGetProperty(ipc, "pause").(bool)
	if paused {
		pauseCmd(ipc)
	}
}

type addCmdMode string

const (
	appendACM  addCmdMode = "append"
	replaceACM addCmdMode = "replace"
)

// addCmd adds tracks from args; if there's DIR in args, adds it as playlist
func addCmd(ipc net.Conn, mode addCmdMode, args ...string) {
	if len(args) == 0 {
		usage()
	}
	for _, fpath := range args {
		// TODO: replace with walk and looking on actual extension
		ext := filepath.Ext(fpath)
		if ext == "" {
			ipcSendCmd(ipc, fmt.Sprintf("loadlist %q %s", fpath, mode))
		} else {
			ipcSendCmd(ipc, fmt.Sprintf("loadfile %q %s", fpath, mode))
		}
	}
}

type playlistEntry struct {
	Current  bool   `json:"current"`
	Filename string `json:"filename"`
}

// playlistCmd shows current playlist; if "raw" is specified, show it in raw format; if INDEX is specified, change position to INDEX
func playlistCmd(ipc net.Conn, args ...string) {
	rawPlaylistData, err := json.Marshal(mpvGetProperty(ipc, "playlist"))
	check(err)
	var playlistData []playlistEntry
	err = json.Unmarshal(rawPlaylistData, &playlistData)
	check(err)

	if len(args) == 0 {
		padding := numDigits(len(playlistData)) + 2
		for i, v := range playlistData {
			toPrint := fmt.Sprintf("%*d\t%v", padding, i, filepath.Base(v.Filename))
			if v.Current {
				r := []rune(toPrint)
				r[0] = '>'
				toPrint = string(r)
			}
			fmt.Println(toPrint)
		}
	} else if args[0] == "raw" {
		for _, v := range playlistData {
			fmt.Println(v.Filename)
		}
	} else {
		pos, err := strconv.Atoi(args[0])
		check(err)
		if pos < 0 || pos >= len(playlistData) {
			die("%v: incorrect playlist position %d\n", os.Args[0], pos)
		}
		ipcSendCmd(ipc, fmt.Sprintf("playlist-play-index %d", pos))
	}
}

// pauseCmd toggles pause for current position on the playlist
func pauseCmd(ipc net.Conn) {
	ipcSendCmd(ipc, "cycle pause")
}

// nextCmd change current position on the playlist to the next track
func nextCmd(ipc net.Conn) {
	ipcSendCmd(ipc, "playlist-next")
}

// nextCmd change current position on the playlist to the previous track
func prevCmd(ipc net.Conn) {
	ipcSendCmd(ipc, "playlist-prev")
}

// loopCmd toggles loop for current playlist track
func loopCmd(ipc net.Conn) {
	ipcSendCmd(ipc, "cycle-values loop-file 'inf' 'no'")
}

// killCmd kills current mpv session and deletes programDirPath
func killCmd(pidPath string) {
	data, err := os.ReadFile(pidPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		die("%v: couldn't read file %q: %v\n", os.Args[0], pidPath, err)
	}
	pid, err := strconv.Atoi(string(data))
	check(err)

	err = syscall.Kill(pid, syscall.SIGKILL)
	check(err)

	err = os.RemoveAll(programDirPath)
	check(err)
}

// customCmd executes arbitary command specified in args
func customCmd(ipc net.Conn, args ...string) {
	if len(args) == 0 {
		usage()
	}
	ipcSendCmd(ipc, args[0])
	fmt.Println(string(ipcReceiveMsg(ipc)))
}

type mpvCommandResult struct {
	Data      any    `json:"data"`
	Error     string `json:"error"`
	RequestId int    `json:"request_id"`
}

func mpvGetProperty(ipc net.Conn, propertyName string) any {
	ipcSendCmd(ipc, fmt.Sprintf(`{ "command": ["get_property", "%s"] }`, propertyName))

	dec := json.NewDecoder(bytes.NewReader(ipcReceiveMsg(ipc)))
	ok := false
	var res mpvCommandResult
	for {
		err := dec.Decode(&res)
		if err == nil {
			ok = true
			break
		}

		if err == io.EOF {
			break
		}
	}
	if !ok {
		die("%v: couldn't get property %q\n", os.Args[0], propertyName)
	}

	return res.Data
}

func ipcSendCmd(ipc net.Conn, cmd string) {
	cmdByte := []byte(fmt.Sprintf("%s\n", cmd))
	n, err := ipc.Write(cmdByte)
	check(err)

	if n != len(cmdByte) {
		die("%v: incorrect arg %q: length too long\n", os.Args[0], cmd)
	}
}

func ipcReceiveMsg(ipc net.Conn) []byte {
	buf := make([]byte, 4096)
	res := make([]byte, 0, 4096)

	err := ipc.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	check(err)

	for {
		n, err := ipc.Read(buf)
		if err != nil {
			break
		}
		res = append(res, buf[:n]...)
	}

	return res
}

func usage() {
	usageStr := "usage: %v COMMAND [args...]\n" +
		"commands:\n" +
		"\tplay [FILE...]      \tplay the current playlist from the beginning or replace it with FILE; FILE could be track or playlist\n" +
		"\tadd FILE...         \tadd FILE to the current playlist; FILE could be track or playlist\n" +
		"\tplaylist [raw|INDEX]\tshow current playlist; if raw is specified, show it in raw format; if INDEX is specified, change position to INDEX\n" +
		"\tpause               \ttoggle pause\n" +
		"\tnext                \tgo to the next track in the current playlist\n" +
		"\tprev                \tgo to the prev track in the current playlist\n" +
		"\tloop                \ttoogle loop for the current track\n" +
		"\tkill                \tclean up and kill running mpv-mp instance\n" +
		"\t-                   \tsend custom command to mpv\n"
	die(usageStr, os.Args[0])
}

func numDigits(i int) int {
	return len(strconv.Itoa(i))
}

func check(err error) {
	if err != nil {
		die("%v: unexpected error: %v\n", os.Args[0], err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
