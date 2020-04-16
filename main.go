// Copyright (C) 2020  Davide Depau <davide@depau.eu>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/op/go-logging"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

var log = logging.MustGetLogger("docker-platformify")
var format = logging.MustStringFormatter(
	`%{color}%{shortfunc:-15.15s} ▶ %{level:.5s}%{color:reset} %{message}`,
)

func forwardAll(srcConn net.Conn, dstConn net.Conn) {
	buffer := make([]byte, 4096)
	var (
		readErr      error
		writeErr     error
		bytesRead    int
		bytesWritten int
	)
	for {
		err := srcConn.SetReadDeadline(time.Now().Add(time.Millisecond * 50))
		if err != nil {
			log.Error("failed to set socket timeout:", err)
			break
		}

		bytesRead, readErr = srcConn.Read(buffer)
		readBuf := buffer[:bytesRead]
		toWrite := bytesRead

		if readErr != nil {
			if err, ok := readErr.(net.Error); ok && err.Timeout() {
				if bytesRead == 0 {
					continue
				} else {
					readErr = nil
				}
			}
		}

		log.Debug("D -> C", string(buffer))

		for toWrite > 0 {
			bytesWritten, writeErr = dstConn.Write(readBuf)
			toWrite -= bytesWritten
			if writeErr != nil {
				break
			}
		}
		if writeErr != nil || readErr != nil {
			break
		}
	}

	if readErr != nil && readErr != io.EOF {
		if !strings.HasSuffix(readErr.Error(), "use of closed network connection") {
			log.Error("error while reading from docker socket:", readErr)
		}
	}
	if writeErr != nil {
		log.Error("error while writing to client socket:", writeErr)
	}
	if err := dstConn.Close(); err != nil {
		log.Error("unable to close client connection:", err)
	} else {
		log.Info("closed docker -> client")
	}
}

// Inject the platform field into the query parameters without actually parsing
// the full HTTP request
func injectPlatform(buffer []byte, platform string) (injected []byte, err error) {
	parts := bytes.SplitN(buffer, []byte(" "), 3)
	if len(parts) < 3 {
		err = errors.New("invalid HTTP request")
		return
	}
	method := parts[0]
	rawUrl := parts[1]
	version := parts[2]

	u, err := url.Parse(string(rawUrl))
	if err != nil {
		return
	}

	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return
	}

	if _, ok := query["platform"]; ok {
		query.Del("platform")
	}

	query.Add("platform", platform)
	u.RawQuery = query.Encode()

	injUrl := []byte(u.String())

	return bytes.Join([][]byte{method, injUrl, version}, []byte(" ")), nil
}

func sendAll(buffer *[]byte, conn net.Conn) (err error) {
	toWrite := len(*buffer)
	for toWrite > 0 {
		bytesWritten, err := conn.Write(*buffer)
		toWrite -= bytesWritten
		if err != nil {
			return err
		}
	}
	return nil
}

func handleConnection(conn net.Conn, dockerSock string, platform string) {
	buffer := make([]byte, 4096)
	dockerConn, err := net.Dial("unix", dockerSock)
	if err != nil {
		log.Error("unable to connect to Docker socket:", err)
		return
	}

	var (
		readErr  error
		writeErr error
	)
	dirtyBytes := 0
	bytesRead := 0

	go forwardAll(dockerConn, conn)

	for {

		err := conn.SetReadDeadline(time.Now().Add(time.Millisecond * 50))
		if err != nil {
			log.Error("failed to set socket timeout:", err)
			break
		}
		offsetBuf := buffer[dirtyBytes:]
		bytesRead, readErr = conn.Read(offsetBuf)
		bytesRead += dirtyBytes
		dirtyBytes = 0

		if readErr != nil {
			if err, ok := readErr.(net.Error); ok && err.Timeout() {
				if bytesRead == 0 {
					continue
				} else {
					readErr = nil
				}
			}
		}

		readBuf := buffer[:bytesRead]

		if bytes.Contains(readBuf, []byte("POST")) && bytes.Contains(readBuf, []byte("/images/create")) {
			index := bytes.Index(buffer, []byte("POST"))

			if index > 0 {
				// Copy all data before "POST" into a new readBuf to be sent; move the rest to the beginning of buffer
				// so we can process it in the next run
				dirtyBytes = bytesRead - index
				readBuf = make([]byte, index)
				copy(readBuf, buffer[:index])
				for i := 0; i < dirtyBytes; i++ {
					buffer[i] = buffer[index+i]
				}
			} else if index == 0 {
				// Find the end of the line and inject it; then send it and copy the rest of the buffer to the beginning
				// so we can send it in the next run
				index = bytes.Index(readBuf, []byte("\n"))
				if index < 0 {
					log.Warning("tried to inject request, but it's either invalid or too long")
				} else {
					toInjectBuf := readBuf[:index]
					injectedBuf, err := injectPlatform(toInjectBuf, platform)
					if err == nil {
						log.Info("injected 'docker image create/pull' command")

						readBuf = injectedBuf
						dirtyBytes = bytesRead - index
						for i := 0; i < dirtyBytes; i++ {
							buffer[i] = buffer[index+i]
						}
					} else {
						log.Warning("unable to inject HTTP request, sending as is: '%s'; %s\n", toInjectBuf, err)
						err = nil
					}
				}
			}
		}
		log.Debug("C -> D", string(readBuf))

		writeErr = sendAll(&readBuf, dockerConn)

		if readErr != nil || writeErr != nil {
			break
		}
	}

	if readErr != nil && readErr != io.EOF {
		log.Error("error while reading from client socket:", readErr)
	}
	if writeErr != nil {
		log.Error("error while writing to docker socket:", writeErr)
	}

	if err := dockerConn.Close(); err != nil {
		log.Error("unable to close docker connection:", err)
	} else {
		log.Info("closed client -> docker")
	}
}

func ensureSocketDoesNotExist(proxySock string) error {
	// Delete socket if it exists
	if stat, err := os.Stat(proxySock); err != nil && !os.IsNotExist(err) {
		// Stat failed, "proxySock" appears to exist in the filesystem
		return errors.New(fmt.Sprintf("unable to stat proxy socket: %v", err))
	} else if stat != nil {
		// Stat didn't fail, "proxySock" exists
		if sysStat, ok := stat.Sys().(*syscall.Stat_t); ok {
			if (sysStat.Mode & syscall.S_IFMT) == syscall.S_IFSOCK {
				// "proxySock" is effectively a socket, we'll remove it
				if err := os.Remove(proxySock); err != nil {
					return errors.New(fmt.Sprintf("proxy socket exists and it could not be removed: %v", err))
				} else {
					log.Info("removed old proxy socket")
					return nil
				}
			} else {
				return errors.New(fmt.Sprintf("proxy socket '%s' exists and is not a socket", proxySock))
			}
		} else {
			// proxySock exists but we weren't able to check what it is.
			// We're not going to delete it since it might be some important document
			return errors.New(fmt.Sprintf("proxy socket '%s' exists in filesystem", proxySock))
		}
	}
	return nil
}

func main() {
	fmt.Println(
		"docker-platformify  Copyright (C) 2020  Davide Depau <davide@depau.eu>\n" +
			"This program comes with ABSOLUTELY NO WARRANTY; This is free software,\n" +
			"and you are welcome to redistribute it under certain conditions.",
	)

	if len(os.Args) < 4 {
		_, _ = fmt.Fprintf(os.Stderr, "Usage: %s <docker socket> <proxied socket> <platform string> [log level]\n", os.Args[0])
		_, _ = fmt.Fprintln(os.Stderr, "Log level must be one of: CRITICAL, ERROR, WARNING, NOTICE, INFO, DEBUG; default INFO")
		os.Exit(1)
	}

	dockerSock := os.Args[1]
	proxySock := os.Args[2]
	platform := os.Args[3]

	// Setup logging
	if len(os.Args) > 4 {
		level, err := logging.LogLevel(os.Args[4])
		if err != nil {
			fmt.Println("unable to set log level:", err)
			os.Exit(1)
		}
		logging.SetLevel(level, "docker-platformify")
	} else {
		logging.SetLevel(logging.INFO, "docker-platformify")
	}
	logging.SetFormatter(format)

	// Ensure the socket either does not exist or can be removed
	// Make the program fail otherwise
	if err := ensureSocketDoesNotExist(proxySock); err != nil {
		log.Fatal(err)
	}

	ln, err := net.Listen("unix", proxySock)
	if err != nil {
		log.Fatal("unable to listen to Unix socket:", err)
	}
	log.Notice("listening on proxy socket", proxySock)
	for {
		if conn, err := ln.Accept(); err != nil {
			log.Error("unable to accept connection:", err)
		} else {
			log.Info("new connection to proxy socket")
			go handleConnection(conn, dockerSock, platform)
		}
	}
}
