/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysql

import (
	"bytes"
	"encoding/json"
	"flag"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Sndav/vitess/go/vt/log"
	querypb "github.com/Sndav/vitess/go/vt/proto/query"
	"github.com/Sndav/vitess/go/vt/proto/vtrpc"
	"github.com/Sndav/vitess/go/vt/vterrors"
)

var (
	mysqlAuthServerStaticFile           = flag.String("mysql_auth_server_static_file", "", "JSON File to read the users/passwords from.")
	mysqlAuthServerStaticString         = flag.String("mysql_auth_server_static_string", "", "JSON representation of the users/passwords config.")
	mysqlAuthServerStaticReloadInterval = flag.Duration("mysql_auth_static_reload_interval", 0, "Ticker to reload credentials")
)

const (
	localhostName = "localhost"
)

// AuthServerStatic implements AuthServer using a static configuration.
type AuthServerStatic struct {
	// Method can be set to:
	// - MysqlNativePassword
	// - MysqlClearPassword
	// - MysqlDialog
	// It defaults to MysqlNativePassword.
	Method string
	// This mutex helps us prevent data races between the multiple updates of Entries.
	mu sync.Mutex
	// Entries contains the users, passwords and user data.
	Entries map[string][]*AuthServerStaticEntry
}

// AuthServerStaticEntry stores the values for a given user.
type AuthServerStaticEntry struct {
	// MysqlNativePassword is generated by password hashing methods in MySQL.
	// These changes are illustrated by changes in the result from the PASSWORD() function
	// that computes password hash values and in the structure of the user table where passwords are stored.
	// mysql> SELECT PASSWORD('mypass');
	// +-------------------------------------------+
	// | PASSWORD('mypass')                        |
	// +-------------------------------------------+
	// | *6C8989366EAF75BB670AD8EA7A7FC1176A95CEF4 |
	// +-------------------------------------------+
	// MysqlNativePassword's format looks like "*6C8989366EAF75BB670AD8EA7A7FC1176A95CEF4", it store a hashing value.
	// Use MysqlNativePassword in auth config, maybe more secure. After all, it is cryptographic storage.
	MysqlNativePassword string
	Password            string
	UserData            string
	SourceHost          string
	Groups              []string
}

// InitAuthServerStatic Handles initializing the AuthServerStatic if necessary.
func InitAuthServerStatic() {
	// Check parameters.
	if *mysqlAuthServerStaticFile == "" && *mysqlAuthServerStaticString == "" {
		// Not configured, nothing to do.
		log.Infof("Not configuring AuthServerStatic, as mysql_auth_server_static_file and mysql_auth_server_static_string are empty")
		return
	}
	if *mysqlAuthServerStaticFile != "" && *mysqlAuthServerStaticString != "" {
		// Both parameters specified, can only use one.
		log.Exitf("Both mysql_auth_server_static_file and mysql_auth_server_static_string specified, can only use one.")
	}

	// Create and register auth server.
	RegisterAuthServerStaticFromParams(*mysqlAuthServerStaticFile, *mysqlAuthServerStaticString)
}

// NewAuthServerStatic returns a new empty AuthServerStatic.
func NewAuthServerStatic() *AuthServerStatic {
	return &AuthServerStatic{
		Method:  MysqlNativePassword,
		Entries: make(map[string][]*AuthServerStaticEntry),
	}
}

// RegisterAuthServerStaticFromParams creates and registers a new
// AuthServerStatic, loaded for a JSON file or string. If file is set,
// it uses file. Otherwise, load the string. It log.Exits out in case
// of error.
func RegisterAuthServerStaticFromParams(file, str string) {
	authServerStatic := NewAuthServerStatic()

	authServerStatic.loadConfigFromParams(file, str)

	if len(authServerStatic.Entries) <= 0 {
		log.Exitf("Failed to populate entries from file: %v", file)
	}
	authServerStatic.installSignalHandlers()

	// And register the server.
	RegisterAuthServerImpl("static", authServerStatic)
}

func (a *AuthServerStatic) loadConfigFromParams(file, str string) {
	jsonConfig := []byte(str)
	if file != "" {
		data, err := ioutil.ReadFile(file)
		if err != nil {
			log.Errorf("Failed to read mysql_auth_server_static_file file: %v", err)
			return
		}
		jsonConfig = data
	}

	entries := make(map[string][]*AuthServerStaticEntry)
	if err := parseConfig(jsonConfig, &entries); err != nil {
		log.Errorf("Error parsing auth server config: %v", err)
		return
	}

	a.mu.Lock()
	a.Entries = entries
	a.mu.Unlock()
}

func (a *AuthServerStatic) installSignalHandlers() {
	if *mysqlAuthServerStaticFile == "" {
		return
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP)
	go func() {
		for range sigChan {
			a.loadConfigFromParams(*mysqlAuthServerStaticFile, "")
		}
	}()

	// If duration is set, it will reload configuration every interval
	if *mysqlAuthServerStaticReloadInterval > 0 {
		ticker := time.NewTicker(*mysqlAuthServerStaticReloadInterval)
		go func() {
			for {
				select {
				case <-ticker.C:
					if *mysqlAuthServerStaticReloadInterval <= 0 {
						ticker.Stop()
						return
					}
					sigChan <- syscall.SIGHUP
				}
			}
		}()
	}
}

func parseConfig(jsonConfig []byte, config *map[string][]*AuthServerStaticEntry) error {
	decoder := json.NewDecoder(bytes.NewReader(jsonConfig))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(config); err != nil {
		// Couldn't parse, will try to parse with legacy config
		return parseLegacyConfig(jsonConfig, config)
	}
	return validateConfig(*config)
}

func parseLegacyConfig(jsonConfig []byte, config *map[string][]*AuthServerStaticEntry) error {
	// legacy config doesn't have an array
	legacyConfig := make(map[string]*AuthServerStaticEntry)
	decoder := json.NewDecoder(bytes.NewReader(jsonConfig))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacyConfig); err != nil {
		return err
	}
	log.Warningf("Config parsed using legacy configuration. Please update to the latest format: {\"user\":[{\"Password\": \"xxx\"}, ...]}")
	for key, value := range legacyConfig {
		(*config)[key] = append((*config)[key], value)
	}
	return nil
}

func validateConfig(config map[string][]*AuthServerStaticEntry) error {
	for _, entries := range config {
		for _, entry := range entries {
			if entry.SourceHost != "" && entry.SourceHost != localhostName {
				return vterrors.Errorf(vtrpc.Code_INVALID_ARGUMENT, "invalid SourceHost found (only localhost is supported): %v", entry.SourceHost)
			}
		}
	}
	return nil
}

// AuthMethod is part of the AuthServer interface.
func (a *AuthServerStatic) AuthMethod(user string) (string, error) {
	return a.Method, nil
}

// Salt is part of the AuthServer interface.
func (a *AuthServerStatic) Salt() ([]byte, error) {
	return NewSalt()
}

// ValidateHash is part of the AuthServer interface.
func (a *AuthServerStatic) ValidateHash(salt []byte, user string, authResponse []byte, remoteAddr net.Addr) (Getter, error) {
	a.mu.Lock()
	entries, ok := a.Entries[user]
	a.mu.Unlock()

	if !ok {
		return &StaticUserData{}, NewSQLError(ERAccessDeniedError, SSAccessDeniedError, "Access denied for user '%v'", user)
	}

	for _, entry := range entries {
		if entry.MysqlNativePassword != "" {
			isPass := isPassScrambleMysqlNativePassword(authResponse, salt, entry.MysqlNativePassword)
			if matchSourceHost(remoteAddr, entry.SourceHost) && isPass {
				return &StaticUserData{entry.UserData, entry.Groups}, nil
			}
		} else {
			computedAuthResponse := ScramblePassword(salt, []byte(entry.Password))
			// Validate the password.
			if matchSourceHost(remoteAddr, entry.SourceHost) && bytes.Equal(authResponse, computedAuthResponse) {
				return &StaticUserData{entry.UserData, entry.Groups}, nil
			}
		}
	}
	return &StaticUserData{}, NewSQLError(ERAccessDeniedError, SSAccessDeniedError, "Access denied for user '%v'", user)
}

// Negotiate is part of the AuthServer interface.
// It will be called if Method is anything else than MysqlNativePassword.
// We only recognize MysqlClearPassword and MysqlDialog here.
func (a *AuthServerStatic) Negotiate(c *Conn, user string, remoteAddr net.Addr) (Getter, error) {
	// Finish the negotiation.
	password, err := AuthServerNegotiateClearOrDialog(c, a.Method)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	entries, ok := a.Entries[user]
	a.mu.Unlock()

	if !ok {
		return &StaticUserData{}, NewSQLError(ERAccessDeniedError, SSAccessDeniedError, "Access denied for user '%v'", user)
	}
	for _, entry := range entries {
		// Validate the password.
		if matchSourceHost(remoteAddr, entry.SourceHost) && entry.Password == password {
			return &StaticUserData{entry.UserData, entry.Groups}, nil
		}
	}
	return &StaticUserData{}, NewSQLError(ERAccessDeniedError, SSAccessDeniedError, "Access denied for user '%v'", user)
}

func matchSourceHost(remoteAddr net.Addr, targetSourceHost string) bool {
	// Legacy support, there was not matcher defined default to true
	if targetSourceHost == "" {
		return true
	}
	switch remoteAddr.(type) {
	case *net.UnixAddr:
		if targetSourceHost == localhostName {
			return true
		}
	}
	return false
}

// StaticUserData holds the username and groups
type StaticUserData struct {
	username string
	groups   []string
}

// Get returns the wrapped username and groups
func (sud *StaticUserData) Get() *querypb.VTGateCallerID {
	return &querypb.VTGateCallerID{Username: sud.username, Groups: sud.groups}
}
