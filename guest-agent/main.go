// guest-agent runs inside the Linux VM and accepts control commands over
// virtio-vsock. It is intentionally small and static-linked.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"

	"github.com/mdlayher/vsock"
)

const listenPort = 1024

// Request and Response use a simple length-prefixed JSON protocol.
type Request struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args,omitempty"`
}

type Response struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
	Status   string `json:"status,omitempty"`
}

func main() {
	log.SetPrefix("[guest-agent] ")
	log.Printf("listening on vsock port %d", listenPort)

	l, err := vsock.Listen(listenPort, nil)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()

	for {
		req, err := readRequest(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("read request: %v", err)
			}
			return
		}

		resp := dispatch(req)
		if err := writeResponse(conn, resp); err != nil {
			log.Printf("write response: %v", err)
			return
		}
	}
}

func dispatch(req *Request) Response {
	switch req.Cmd {
	case "health", "status":
		return Response{Status: "ok"}
	case "exec":
		return runExec(req.Args)
	default:
		return Response{Error: fmt.Sprintf("unknown command: %s", req.Cmd), ExitCode: 1}
	}
}

func runExec(args []string) Response {
	if len(args) == 0 {
		return Response{Error: "no command", ExitCode: 1}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Response{Error: err.Error(), ExitCode: 1}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Response{Error: err.Error(), ExitCode: 1}
	}
	if err := cmd.Start(); err != nil {
		return Response{Error: err.Error(), ExitCode: 1}
	}

	out, _ := io.ReadAll(stdout)
	errOut, _ := io.ReadAll(stderr)
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return Response{
		Stdout:   string(out),
		Stderr:   string(errOut),
		ExitCode: exitCode,
	}
}

func readRequest(r io.Reader) (*Request, error) {
	br := bufio.NewReader(r)
	var length uint32
	if err := binary.Read(br, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	if length > 1<<20 {
		return nil, fmt.Errorf("request too large: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func writeResponse(w io.Writer, resp Response) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
