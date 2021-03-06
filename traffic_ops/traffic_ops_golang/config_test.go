package main

/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/basho/riak-go-client"
)

const (
	logError   = "/var/log/traffic_ops/error.log"
	logWarning = "/var/log/traffic_ops/warning.log"
	logInfo    = "/var/log/traffic_ops/info.log"
	logDebug   = "/var/log/traffic_ops/debug.log"
	logEvent   = "/var/log/traffic_ops/event.log"
)

var debugLogging = flag.Bool("debug", false, "enable debug logging in test")

var cfg = Config{
	URL:             nil,
	ConfigHypnotoad: ConfigHypnotoad{},
	ConfigTrafficOpsGolang: ConfigTrafficOpsGolang{
		LogLocationError:   logError,
		LogLocationWarning: logWarning,
		LogLocationInfo:    logInfo,
		LogLocationDebug:   logDebug,
		LogLocationEvent:   logEvent,
	},
	DB:      ConfigDatabase{},
	Secrets: []string{},
}

func TestLogLocation(t *testing.T) {
	if cfg.ErrorLog() != logError {
		t.Error("ErrorLog should be ", logError)
	}
	if cfg.WarningLog() != logWarning {
		t.Error("WarningLog should be ", logWarning)
	}
	if cfg.InfoLog() != logInfo {
		t.Error("InfoLog should be ", logInfo)
	}
	if cfg.DebugLog() != logDebug {
		t.Error("DebugLog should be ", logDebug)
	}
	if cfg.EventLog() != logEvent {
		t.Error("EventLog should be ", logEvent)
	}
}

func tempFileWith(content []byte) (string, error) {
	tmpfile, err := ioutil.TempFile("", "badcdn.conf")
	if err != nil {
		return "", err
	}
	if _, err := tmpfile.Write(content); err != nil {
		return "", err
	}
	if err := tmpfile.Close(); err != nil {
		return "", err
	}
	return tmpfile.Name(), nil
}

const (
	goodConfig = `
{
	"hypnotoad" : {
		"listen" : [
			"https://[::]:60443?cert=/etc/pki/tls/certs/localhost.crt&key=/etc/pki/tls/private/localhost.key&verify=0x00&ciphers=AES128-GCM-SHA256:HIGH:!RC4:!MD5:!aNULL:!EDH:!ED"
		],
		"user" : "trafops",
		"group" : "trafops",
		"heartbeat_timeout" : 20,
		"pid_file" : "/var/run/traffic_ops.pid",
		"workers" : 12
	},
	"traffic_ops_golang" : {
		"port" : "443",
		"proxy_timeout" : 60,
		"proxy_keep_alive" : 60,
		"proxy_tls_timeout" : 60,
		"proxy_read_header_timeout" : 60,
		"read_timeout" : 60,
		"read_header_timeout" : 60,
		"write_timeout" : 60,
		"idle_timeout" : 60,
		"log_location_error": "stderr",
		"log_location_warning": "stdout",
		"log_location_info": "stdout",
		"log_location_debug": "stdout",
		"log_location_event": "access.log"
	},
	"cors" : {
		"access_control_allow_origin" : "*"
	},
	"to" : {
		"base_url" : "http://localhost:3000",
		"email_from" : "no-reply@traffic-ops-domain.com",
		"no_account_found_msg" : "A Traffic Ops user account is required for access. Please contact your Traffic Ops user administrator."
	},
	"portal" : {
		"base_url" : "http://localhost:8080/!#/",
		"email_from" : "no-reply@traffic-portal-domain.com",
		"pass_reset_path" : "user",
		"user_register_path" : "user"
	},
	"secrets" : [
		"mONKEYDOmONKEYSEE."
	],
	"geniso" : {
		"iso_root_path" : "/opt/traffic_ops/app/public"
	},
	"inactivity_timeout" : 60
}
`

	goodDbConfig = `
{
	"description": "Local PostgreSQL database on port 5432",
	"dbname": "traffic_ops",
	"hostname": "localhost",
	"user": "traffic_ops",
	"password": "password",
	"port": "5432",
	"type": "Pg"
}
`

	goodRiakConfig = `
	   {
	       "user": "riakuser",
	       "password": "password",
	       "tlsConfig": {
	           "insecureSkipVerify": true
	       }
	   }
	   	`
)

func TestLoadConfig(t *testing.T) {
	var err error
	var exp string

	// set up config paths
	badPath := "/invalid-path/no-file-exists-here"
	badCfg, err := tempFileWith([]byte("no way this is valid json..."))
	if err != nil {
		t.Errorf("cannot create temp file: %v", err)
	}
	defer os.Remove(badCfg) // clean up

	goodCfg, err := tempFileWith([]byte(goodConfig))
	if err != nil {
		t.Errorf("cannot create temp file: %v", err)
	}
	defer os.Remove(goodCfg) // clean up

	goodDbCfg, err := tempFileWith([]byte(goodDbConfig))
	if err != nil {
		t.Errorf("cannot create temp file: %v", err)
	}
	defer os.Remove(goodDbCfg) // clean up

	goodRiakCfg, err := tempFileWith([]byte(goodRiakConfig))
	if err != nil {
		t.Errorf("cannot create temp file: %v", err)
	}
	defer os.Remove(goodRiakCfg) // clean up

	// test bad paths
	_, err = LoadConfig(badPath, badPath, badPath)
	exp = fmt.Sprintf("reading CDN conf '%s'", badPath)
	if !strings.HasPrefix(err.Error(), exp) {
		t.Error("expected", exp, "got", err)
	}

	// bad json in cdn.conf
	_, err = LoadConfig(badCfg, badCfg, badPath)
	exp = fmt.Sprintf("unmarshalling '%s'", badCfg)
	if !strings.HasPrefix(err.Error(), exp) {
		t.Error("expected", exp, "got", err)
	}

	// good cdn.conf, bad db conf
	_, err = LoadConfig(goodCfg, badPath, badPath)
	exp = fmt.Sprintf("reading db conf '%s'", badPath)
	if !strings.HasPrefix(err.Error(), exp) {
		t.Error("expected", exp, "got", err)
	}

	// good cdn.conf,  bad json in database.conf
	_, err = LoadConfig(goodCfg, badCfg, badPath)
	exp = fmt.Sprintf("unmarshalling '%s'", badCfg)
	if !strings.HasPrefix(err.Error(), exp) {
		t.Error("expected", exp, "got", err)
	}

	// good cdn.conf,  good database.conf
	cfg, err = LoadConfig(goodCfg, goodDbCfg, goodRiakCfg)
	if err != nil {
		t.Error("Good config -- unexpected error ", err)
	}

	expectedRiak := riak.AuthOptions{User: "riakuser", Password: "password", TlsConfig: &tls.Config{InsecureSkipVerify: true}}

	if cfg.RiakAuthOptions.User != expectedRiak.User || cfg.RiakAuthOptions.Password != expectedRiak.Password || !reflect.DeepEqual(cfg.RiakAuthOptions.TlsConfig, expectedRiak.TlsConfig) {
		t.Error(fmt.Printf("Error parsing riak conf expected: %++v but got: %++v\n", expectedRiak, cfg.RiakAuthOptions))
	}

	if *debugLogging {
		fmt.Printf("Cfg: %+v\n", cfg)
	}

	if cfg.CertPath != "/etc/pki/tls/certs/localhost.crt" {
		t.Error("Expected KeyPath() == /etc/pki/tls/private/localhost.key")
	}

	if cfg.KeyPath != "/etc/pki/tls/private/localhost.key" {
		t.Error("Expected KeyPath() == /etc/pki/tls/private/localhost.key")
	}
}
