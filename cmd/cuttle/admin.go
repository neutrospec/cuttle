package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"syscall"

	"golang.org/x/term"

	"squid/internal/admin"
	"squid/internal/paths"
)

const adminUsage = "usage: cuttle admin passwd [--stdin]"

// cmdAdmin runs cuttle admin passwd (design 0005 D6, 0006 D3): on a
// terminal it prompts twice without echo; with --stdin it reads one line.
func cmdAdmin(args []string, lookup env, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "passwd" {
		fmt.Fprintln(stderr, adminUsage)
		return exitUsage
	}
	fl := newFlags("admin passwd", stderr)
	fromStdin := fl.Bool("stdin", false, "read the password as one line from standard input")
	if fl.Parse(args[1:]) != nil || fl.NArg() != 0 {
		fmt.Fprintln(stderr, adminUsage)
		return exitUsage
	}
	p, err := paths.Resolve(lookup)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	var pw []byte
	if *fromStdin {
		pw, err = readPasswordLine(stdin)
	} else {
		f, ok := stdin.(*os.File)
		if !ok || !term.IsTerminal(int(f.Fd())) {
			fmt.Fprintln(stderr, "cuttle: standard input is not a terminal; use --stdin to read the password from it")
			return exitFailure
		}
		pw, err = promptPassword(int(f.Fd()), stderr)
	}
	if err == nil {
		err = admin.CheckPasswordLength(len(pw))
	}
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	syscall.Umask(0o077)
	phc, err := admin.HashPassword(pw)
	if err == nil {
		err = admin.WritePasswordFile(p.Password.Path, phc)
	}
	if err != nil {
		fmt.Fprintln(stderr, "cuttle: set the admin password:", err)
		return exitFailure
	}
	fmt.Fprintln(stderr, "cuttle: admin password set; existing admin sessions end at their next request")
	fmt.Fprintln(stdout, p.Password.Path)
	return exitOK
}

// readPasswordLine reads one line without its line ending. A line longer
// than any valid password is refused without reading further.
func readPasswordLine(r io.Reader) ([]byte, error) {
	br := bufio.NewReaderSize(r, 4096)
	line, err := br.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return nil, fmt.Errorf("the password must be at most %d bytes", admin.MaxPasswordLen)
	case err != nil && !errors.Is(err, io.EOF):
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return bytes.Clone(line), nil
}

// promptPassword asks twice without echo.
func promptPassword(fd int, stderr io.Writer) ([]byte, error) {
	fmt.Fprint(stderr, "New admin password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(stderr)
	if err != nil {
		return nil, err
	}
	if err := admin.CheckPasswordLength(len(first)); err != nil {
		return nil, err
	}
	fmt.Fprint(stderr, "Repeat the password: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(stderr)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(first, second) {
		return nil, errors.New("the passwords do not match; nothing was changed")
	}
	return first, nil
}

// startAdmin binds the admin listener and serves the admin UI (design 0005
// D5). A listener that cannot bind is logged, and cuttle runs without the
// admin UI, since forwarding does not depend on it.
func (s *serveState) startAdmin(addr netip.AddrPort, listen func(network, address string) (net.Listener, error)) *http.Server {
	l, err := listen("tcp", addr.String())
	if err != nil {
		s.log.Error("admin listener unavailable; running without the admin UI", "address", addr.String(), "error", err.Error())
		return nil
	}
	// The Host allowlist names the port browsers connect to.
	if ta, ok := l.Addr().(*net.TCPAddr); ok {
		addr = netip.AddrPortFrom(addr.Addr(), uint16(ta.Port))
	}
	srv := admin.New(admin.Options{Listen: addr, PasswordPath: s.paths.Password.Path, Logger: s.log}).Serve(l)
	s.log.Info("admin UI listening", "address", addr.String())
	if problem := admin.PasswordProblem(s.paths.Password.Path); problem != "" {
		s.log.Warn("admin login disabled until cuttle admin passwd runs", "reason", problem, "password", s.paths.Password.Path)
	}
	return srv
}
