// Copyright (C) 2014-2018 Goodrain Co., Ltd.
// RAINBOND, Application Management Platform

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/fatih/structs"
	"github.com/gorilla/websocket"
)

type clientContext struct {
	app        *App
	request    *http.Request
	connection *websocket.Conn
	command    *exec.Cmd
	pty        *os.File
	writeMutex *sync.Mutex
}

const (
	Input          = '0'
	Ping           = '1'
	ResizeTerminal = '2'
)

const (
	Output         = '0'
	Pong           = '1'
	SetWindowTitle = '2'
	SetPreferences = '3'
	SetReconnect   = '4'
)

type argResizeTerminal struct {
	Columns float64
	Rows    float64
}

type ContextVars struct {
	Command    string
	Pid        int
	Hostname   string
	RemoteAddr string
}

func (context *clientContext) goHandleClient() {
	exit := make(chan bool, 2)

	go func() {
		defer func() { exit <- true }()

		context.processSend()
	}()

	go func() {
		defer func() { exit <- true }()

		context.processReceive()
	}()

	go func() {

		<-exit
		context.pty.Close()

		// Even if the PTY has been closed,
		// Read(0 in processSend() keeps blocking and the process doen't exit
		context.command.Process.Signal(syscall.Signal(context.app.options.CloseSignal))

		context.command.Wait()
		context.connection.Close()
		log.Printf("Connection closed: %s", context.request.RemoteAddr)
	}()
}

func (context *clientContext) processSend() {
	if err := context.sendInitialize(); err != nil {
		log.Printf(err.Error())
		return
	}

	buf := make([]byte, 1024)

	for {
		size, err := context.pty.Read(buf)
		if err != nil {
			log.Printf("Command exited for: %s", context.request.RemoteAddr)
			return
		}
		safeMessage := base64.StdEncoding.EncodeToString([]byte(buf[:size]))
		if err = context.write(append([]byte{Output}, []byte(safeMessage)...)); err != nil {
			log.Printf(err.Error())
			return
		}
	}
}

func (context *clientContext) write(data []byte) error {
	context.writeMutex.Lock()
	defer context.writeMutex.Unlock()
	return context.connection.WriteMessage(websocket.TextMessage, data)
}

func (context *clientContext) sendInitialize() error {
	hostname, _ := os.Hostname()
	titleVars := ContextVars{
		Command:    strings.Join(context.app.command, " "),
		Pid:        context.command.Process.Pid,
		Hostname:   hostname,
		RemoteAddr: context.request.RemoteAddr,
	}

	titleBuffer := new(bytes.Buffer)
	if err := context.app.titleTemplate.Execute(titleBuffer, titleVars); err != nil {
		return err
	}
	if err := context.write(append([]byte{SetWindowTitle}, titleBuffer.Bytes()...)); err != nil {
		return err
	}

	prefStruct := structs.New(context.app.options.Preferences)
	prefMap := prefStruct.Map()
	htermPrefs := make(map[string]interface{})
	for key, value := range prefMap {
		rawKey := prefStruct.Field(key).Tag("hcl")
		if _, ok := context.app.options.RawPreferences[rawKey]; ok {
			htermPrefs[strings.Replace(rawKey, "_", "-", -1)] = value
		}
	}
	prefs, err := json.Marshal(htermPrefs)
	if err != nil {
		return err
	}

	if err := context.write(append([]byte{SetPreferences}, prefs...)); err != nil {
		return err
	}
	if context.app.options.EnableReconnect {
		reconnect, _ := json.Marshal(context.app.options.ReconnectTime)
		if err := context.write(append([]byte{SetReconnect}, reconnect...)); err != nil {
			return err
		}
	}
	return nil
}

func (context *clientContext) processReceive() {
	for {
		_, data, err := context.connection.ReadMessage()
		if err != nil {
			log.Print(err.Error())
			return
		}
		if len(data) == 0 {
			log.Print("An error has occured")
			return
		}

		switch data[0] {
		case Input:
			if !context.app.options.PermitWrite {
				break
			}

			_, err := context.pty.Write(data[1:])
			if err != nil {
				return
			}

		case Ping:
			if err := context.write([]byte{Pong}); err != nil {
				log.Print(err.Error())
				return
			}
		case ResizeTerminal:
			var args argResizeTerminal
			err = json.Unmarshal(data[1:], &args)
			if err != nil {
				log.Print("Malformed remote command")
				return
			}

			window := struct {
				row uint16
				col uint16
				x   uint16
				y   uint16
			}{
				uint16(args.Rows),
				uint16(args.Columns),
				0,
				0,
			}
			syscall.Syscall(
				syscall.SYS_IOCTL,
				context.pty.Fd(),
				syscall.TIOCSWINSZ,
				uintptr(unsafe.Pointer(&window)),
			)

		default:
			log.Print("Unknown message type")
			return
		}
	}
}
