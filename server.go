/*
midgaard_matrix_bot, a Matrix bot which sets a bridge to MUD

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/reiver/go-telnet"
)

type ServerConfig struct {
	Address    string `short:"a" long:"address" description:"Local address at which to bind the websocket server" required:"true"`
	TelnetHost string `short:"H" long:"host" description:"Host and port for TinyMUSH" required:"true"`
	ConnectCmd string `short:"c" long:"connect-command" description:"Command used to connect to a user once telnet connection is established."`
}

type ServerState struct {
	config       *ServerConfig
	currentState string
	sendChannel  chan string
	cancelFunc   context.CancelFunc
	mushState    *MushState
}

type MushState struct {
	Players []*MushPlayer `json:"players"`
}

type MushLocation string

type MushPlayer struct {
	Name     string       `json:"name"`
	Location MushLocation `json:"location"`
}

const (
	STATE_NOT_CONNECTED = "not_connected"
	STATE_CONNECTING    = "connecting"
	STATE_LOGGING_IN    = "logging_in"
	STATE_IDLE          = "idle"
	STATE_AWAIT_WHO     = "await_who"
	STATE_AWAIT_LOC     = "await_location"
)

var locationCache map[string]string
var unknownLocations []string

func (l *MushLocation) MarshalJSON() ([]byte, error) {
	loc, ok := locationCache[string(*l)]
	if !ok {
		return []byte(fmt.Sprintf(`"%s"`, string(*l))), nil
	}
	return []byte(fmt.Sprintf(`"%s"`, loc)), nil
}

func (s *ServerState) sendWorker(caller TelnetCaller, ctx context.Context) {

	for {
		select {
		case msg := <-caller.Output:
			s.processMessage(msg)
		case <-caller.ErrorOut:
			log.Default().Println("telnet error")
			s.currentState = STATE_NOT_CONNECTED
			return
		case <-ctx.Done():
			caller.ErrorIn <- errors.New("Cancelled")
			return
		}
	}
}

func (s *ServerState) loopWorker(t *time.Ticker, ctx context.Context) {

	s.processTick(ctx)
	for {
		select {
		case <-t.C:
			s.processTick(ctx)
		case <-ctx.Done():
			log.Println("Context over.")
			t.Stop()
			return
		}
	}
}

func (s *ServerState) connectToTelnet(ctx context.Context) {
	telnetInput, telnetOutput, telnetErrorOut, telnetErrorIn := make(chan string), make(chan string), make(chan string), make(chan error)
	caller := TelnetCaller{
		Input:    telnetInput,
		Output:   telnetOutput,
		ErrorOut: telnetErrorOut,
		ErrorIn:  telnetErrorIn,
	}
	go s.sendWorker(caller, ctx)

	log.Println("Dialing telnet")
	s.sendChannel = telnetInput
	go telnet.DialToAndCall(s.config.TelnetHost, caller)
}

func (s *ServerState) processMessage(message string) {
	switch s.currentState {
	case STATE_CONNECTING:
		log.Println("Logging in...")
		s.currentState = STATE_LOGGING_IN
		s.sendChannel <- s.config.ConnectCmd
	case STATE_LOGGING_IN:
		log.Println("Login successful.")
		s.currentState = STATE_IDLE
	case STATE_AWAIT_WHO:
		s.currentState = STATE_IDLE
		s.processWho(message)
	case STATE_AWAIT_LOC:
		s.currentState = STATE_IDLE
		s.processLocation(message)
	default:
		log.Println("Received unexpected message:")
		log.Println(message)
	}
}

func (s *ServerState) processTick(ctx context.Context) {
	switch s.currentState {
	case STATE_NOT_CONNECTED:
		log.Println("Connecting...")
		s.currentState = STATE_CONNECTING
		s.connectToTelnet(ctx)
	case STATE_IDLE:
		if len(unknownLocations) > 0 {
			s.getLocation()
		} else {
			s.currentState = STATE_AWAIT_WHO
			s.sendChannel <- "who"
		}
	}
}

func (s *ServerState) getLocation() {
	if len(unknownLocations) == 0 {
		return
	}
	unk := unknownLocations[0]
	s.currentState = STATE_AWAIT_LOC
	s.sendChannel <- fmt.Sprintf("\"%s\"[name(%s)]", unk, unk)
}

func (s *ServerState) processLocation(text string) {
	parts := strings.Split(text, "\"")
	if len(parts) != 4 {
		log.Println("Wrong number of say parts")
		log.Println(text)
	}

	locationCache[parts[1]] = parts[2]

	if unknownLocations[0] == parts[1] {
	unknownLocations = unknownLocations[1:]
	}

}

func (s *ServerState) processWho(text string) {
	lines := strings.Split(text, "\n")
	if len(lines) < 3 {
		log.Println("Not enough who lines:")
		return
	}
	if !strings.HasPrefix(lines[0], "Player Name") {
		log.Println("Who does not start right:")
		log.Println(lines[0])
		return
	}
	if !strings.Contains(lines[len(lines)-2], "logged in") {
		log.Println("Who does not end right:")
		log.Println(lines[len(lines)-2])
		return
	}
	newPlayerStatus := make([]*MushPlayer, len(lines)-3)
	ulo := make([]string, 0)
	for i, line := range lines[1 : len(lines)-1] {
		parts := strings.Fields(line)
		if len(parts) != 6 {
			continue
		}
		newPlayerStatus[i] = &MushPlayer{
			Name:     parts[0],
			Location: MushLocation(parts[3]),
		}
		_, ok := locationCache[parts[3]]
		if !ok && !slices.Contains(ulo, parts[3]) {
			ulo = append(ulo, parts[3])
		}
	}
	unknownLocations = ulo
	s.mushState.Players = newPlayerStatus
}

func (s *ServerState) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonBody, err := json.Marshal(*s.mushState)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
	fmt.Fprint(w, string(jsonBody))
}

func initServer(config ServerConfig, ctx context.Context) error {
	_, cancel := context.WithCancel(ctx)
	locationCache = make(map[string]string)
	unknownLocations = make([]string, 0)
	s := ServerState{
		config:       &config,
		currentState: STATE_NOT_CONNECTED,
		cancelFunc:   cancel,
		mushState: &MushState{
			Players: make([]*MushPlayer, 0),
		},
	}

	ticker := time.NewTicker(time.Second * 30)
	go s.loopWorker(ticker, ctx)

	http.HandleFunc("/api", s.serve)
	server := &http.Server{
		Addr:              config.Address,
		ReadHeaderTimeout: 3 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
	cancel()

	return nil
}
