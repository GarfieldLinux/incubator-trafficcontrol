package cache

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
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/apache/incubator-trafficcontrol/lib/go-log"
	"github.com/apache/incubator-trafficcontrol/lib/go-tc"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/dsdata"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/srvhttp"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/todata"
)

// Handler is a cache handler, which fulfills the common/handler `Handler` interface.
type Handler struct {
	resultChan         chan Result
	Notify             int
	ToData             *todata.TODataThreadsafe
	MultipleSpaceRegex *regexp.Regexp
}

func (h Handler) ResultChan() <-chan Result {
	return h.resultChan
}

// NewHandler returns a new cache handler. Note this handler does NOT precomputes stat data before calling ResultChan, and Result.Precomputed will be nil
func NewHandler() Handler {
	return Handler{resultChan: make(chan Result), MultipleSpaceRegex: regexp.MustCompile(" +")}
}

// NewPrecomputeHandler constructs a new cache Handler, which precomputes stat data and populates result.Precomputed before passing to ResultChan.
func NewPrecomputeHandler(toData todata.TODataThreadsafe) Handler {
	return Handler{resultChan: make(chan Result), MultipleSpaceRegex: regexp.MustCompile(" +"), ToData: &toData}
}

// Precompute returns whether this handler precomputes data before passing the result to the ResultChan
func (handler Handler) Precompute() bool {
	return handler.ToData != nil
}

// PrecomputedData represents data parsed and pre-computed from the Result.
type PrecomputedData struct {
	DeliveryServiceStats map[tc.DeliveryServiceName]dsdata.Stat
	OutBytes             int64
	MaxKbps              int64
	Errors               []error
	Reporting            bool
	Time                 time.Time
}

// Result is the data result returned by a cache.
type Result struct {
	ID              tc.CacheName
	Error           error
	Astats          Astats
	Time            time.Time
	RequestTime     time.Duration
	Vitals          Vitals
	PollID          uint64
	PollFinished    chan<- uint64
	PrecomputedData PrecomputedData
	Available       bool
}

// HasStat returns whether the given stat is in the Result.
func (result *Result) HasStat(stat string) bool {
	computedStats := ComputedStats()
	if _, ok := computedStats[stat]; ok {
		return true // health poll has all computed stats
	}
	if _, ok := result.Astats.Ats[stat]; ok {
		return true
	}
	return false
}

// Vitals is the vitals data returned from a cache.
type Vitals struct {
	LoadAvg    float64
	BytesOut   int64
	BytesIn    int64
	KbpsOut    int64
	MaxKbpsOut int64
}

// Stat is a generic stat, including the untyped value and the time the stat was taken.
type Stat struct {
	Time  int64       `json:"time"`
	Value interface{} `json:"value"`
}

// Stats is designed for returning via the API. It contains result history for each cache, as well as common API data.
type Stats struct {
	srvhttp.CommonAPIData
	Caches map[tc.CacheName]map[string][]ResultStatVal `json:"caches"`
}

// Filter filters whether stats and caches should be returned from a data set.
type Filter interface {
	UseStat(name string) bool
	UseCache(name tc.CacheName) bool
	WithinStatHistoryMax(int) bool
}

const nsPerMs = 1000000

type StatComputeFunc func(resultInfo ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{}

// ComputedStats returns a map of cache stats which are computed by Traffic Monitor (rather than returned literally from ATS), mapped to the func to compute them.
func ComputedStats() map[string]StatComputeFunc {
	return map[string]StatComputeFunc{
		"availableBandwidthInKbps": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.Vitals.MaxKbpsOut - info.Vitals.KbpsOut
		},

		"availableBandwidthInMbps": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return (info.Vitals.MaxKbpsOut - info.Vitals.KbpsOut) / 1000
		},
		"bandwidth": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.Vitals.KbpsOut
		},
		"error-string": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			if info.Error != nil {
				return info.Error.Error()
			}
			return "false"
		},
		"isAvailable": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return combinedState.IsAvailable // if the cache is missing, default to false
		},
		"isHealthy": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			if tc.CacheStatusFromString(serverInfo.ServerStatus) == tc.CacheStatusAdminDown {
				return true
			}
			return combinedState.IsAvailable
		},
		"kbps": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.Vitals.KbpsOut
		},
		"loadavg": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.Vitals.LoadAvg
		},
		"maxKbps": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.Vitals.MaxKbpsOut
		},
		"queryTime": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.RequestTime.Nanoseconds() / nsPerMs
		},
		"stateUrl": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return serverProfile.Parameters.HealthPollingURL
		},
		"status": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return serverInfo.ServerStatus
		},
		"system.astatsLoad": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.AstatsLoad
		},
		"system.configReloadRequests": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.ConfigLoadRequest
		},
		"system.configReloads": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.ConfigReloads
		},
		"system.inf.name": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.InfName
		},
		"system.inf.speed": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.InfSpeed
		},
		"system.lastReload": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.LastReload
		},
		"system.lastReloadRequest": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.LastReloadRequest
		},
		"system.notAvailable": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.NotAvailable
		},
		"system.proc.loadavg": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.ProcLoadavg
		},
		"system.proc.net.dev": func(info ResultInfo, serverInfo tc.TrafficServer, serverProfile tc.TMProfile, combinedState tc.IsAvailable) interface{} {
			return info.System.ProcNetDev
		},
	}
}

// StatsMarshall encodes the stats in JSON, encoding up to historyCount of each stat. If statsToUse is empty, all stats are encoded; otherwise, only the given stats are encoded. If wildcard is true, stats which contain the text in each statsToUse are returned, instead of exact stat names. If cacheType is not CacheTypeInvalid, only stats for the given type are returned. If hosts is not empty, only the given hosts are returned.
func StatsMarshall(statResultHistory ResultStatHistory, statInfo ResultInfoHistory, combinedStates tc.CRStates, monitorConfig tc.TrafficMonitorConfigMap, statMaxKbpses Kbpses, filter Filter, params url.Values) ([]byte, error) {
	stats := Stats{
		CommonAPIData: srvhttp.GetCommonAPIData(params, time.Now()),
		Caches:        map[tc.CacheName]map[string][]ResultStatVal{},
	}

	computedStats := ComputedStats()

	// TODO in 1.0, stats are divided into 'location', 'cache', and 'type'. 'cache' are hidden by default.

	for id, combinedStatesCache := range combinedStates.Caches {
		if !filter.UseCache(id) {
			continue
		}

		for stat, vals := range statResultHistory[id] {
			stat = "ats." + stat // TM1 prefixes ATS stats with 'ats.'
			if !filter.UseStat(stat) {
				continue
			}
			historyCount := 1
			for _, val := range vals {
				if !filter.WithinStatHistoryMax(historyCount) {
					break
				}
				if _, ok := stats.Caches[id]; !ok {
					stats.Caches[id] = map[string][]ResultStatVal{}
				}
				stats.Caches[id][stat] = append(stats.Caches[id][stat], val)
				historyCount += int(val.Span)
			}
		}

		serverInfo, ok := monitorConfig.TrafficServer[string(id)]
		if !ok {
			log.Warnf("cache.StatsMarshall server %s missing from monitorConfig\n", id)
		}

		serverProfile, ok := monitorConfig.Profile[serverInfo.Profile]
		if !ok {
			log.Warnf("cache.StatsMarshall server %s missing profile in monitorConfig\n", id)
		}

		for i, resultInfo := range statInfo[id] {
			if !filter.WithinStatHistoryMax(i + 1) {
				break
			}
			if _, ok := stats.Caches[id]; !ok {
				stats.Caches[id] = map[string][]ResultStatVal{}
			}

			t := resultInfo.Time

			for stat, statValF := range computedStats {
				if !filter.UseStat(stat) {
					continue
				}
				stats.Caches[id][stat] = append(stats.Caches[id][stat], ResultStatVal{Val: statValF(resultInfo, serverInfo, serverProfile, combinedStatesCache), Time: t, Span: 1}) // combinedState will default to unavailable
			}
		}
	}

	return json.Marshal(stats)
}

// Handle handles results fetched from a cache, parsing the raw Reader data and passing it along to a chan for further processing.
func (handler Handler) Handle(id string, r io.Reader, reqTime time.Duration, reqEnd time.Time, reqErr error, pollID uint64, pollFinished chan<- uint64) {
	log.Debugf("poll %v %v handle start\n", pollID, time.Now())
	result := Result{
		ID:           tc.CacheName(id),
		Time:         reqEnd,
		RequestTime:  reqTime,
		PollID:       pollID,
		PollFinished: pollFinished,
	}

	if reqErr != nil {
		log.Warnf("%v handler given error '%v'\n", id, reqErr) // error here, in case the thing that called Handle didn't error
		result.Error = reqErr
		handler.resultChan <- result
		return
	}

	if r == nil {
		log.Warnf("%v handle reader nil\n", id)
		result.Error = fmt.Errorf("handler got nil reader")
		handler.resultChan <- result
		return
	}

	result.PrecomputedData.Reporting = true
	result.PrecomputedData.Time = result.Time

	if decodeErr := json.NewDecoder(r).Decode(&result.Astats); decodeErr != nil {
		log.Warnf("%s procnetdev decode error '%v'\n", id, decodeErr)
		result.Error = decodeErr
		handler.resultChan <- result
		return
	}

	if result.Astats.System.ProcNetDev == "" {
		log.Warnf("addkbps %s procnetdev empty\n", id)
	}

	if result.Astats.System.InfSpeed == 0 {
		log.Warnf("addkbps %s inf.speed empty\n", id)
	}

	if reqErr != nil {
		result.Error = reqErr
		log.Errorf("addkbps handle %s error '%v'\n", id, reqErr)
	} else {
		result.Available = true
	}

	if handler.Precompute() {
		result = handler.precompute(result)
	}

	handler.resultChan <- result
}

// outBytes takes the proc.net.dev string, and the interface name, and returns the bytes field
func outBytes(procNetDev, iface string, multipleSpaceRegex *regexp.Regexp) (int64, error) {
	if procNetDev == "" {
		return 0, fmt.Errorf("procNetDev empty")
	}
	if iface == "" {
		return 0, fmt.Errorf("iface empty")
	}
	ifacePos := strings.Index(procNetDev, iface)
	if ifacePos == -1 {
		return 0, fmt.Errorf("interface '%s' not found in proc.net.dev '%s'", iface, procNetDev)
	}

	procNetDevIfaceBytes := procNetDev[ifacePos+len(iface)+1:]
	procNetDevIfaceBytes = strings.TrimLeft(procNetDevIfaceBytes, " ")
	procNetDevIfaceBytes = multipleSpaceRegex.ReplaceAllLiteralString(procNetDevIfaceBytes, " ")
	procNetDevIfaceBytesArr := strings.Split(procNetDevIfaceBytes, " ") // this could be made faster with a custom function (DFA?) that splits and ignores duplicate spaces at the same time
	if len(procNetDevIfaceBytesArr) < 10 {
		return 0, fmt.Errorf("proc.net.dev iface '%v' unknown format '%s'", iface, procNetDev)
	}
	procNetDevIfaceBytes = procNetDevIfaceBytesArr[8]

	return strconv.ParseInt(procNetDevIfaceBytes, 10, 64)
}

// precompute does the calculations which are possible with only this one cache result.
// TODO precompute ResultStatVal
func (handler Handler) precompute(result Result) Result {
	todata := handler.ToData.Get()
	stats := map[tc.DeliveryServiceName]dsdata.Stat{}

	var err error
	if result.PrecomputedData.OutBytes, err = outBytes(result.Astats.System.ProcNetDev, result.Astats.System.InfName, handler.MultipleSpaceRegex); err != nil {
		result.PrecomputedData.OutBytes = 0
		log.Errorf("addkbps %s handle precomputing outbytes '%v'\n", result.ID, err)
	}

	kbpsInMbps := int64(1000)
	result.PrecomputedData.MaxKbps = int64(result.Astats.System.InfSpeed) * kbpsInMbps

	for stat, value := range result.Astats.Ats {
		var err error
		stats, err = processStat(result.ID, stats, todata, stat, value, result.Time)
		if err != nil && err != dsdata.ErrNotProcessedStat {
			log.Infof("precomputing cache %v stat %v value %v error %v", result.ID, stat, value, err)
			result.PrecomputedData.Errors = append(result.PrecomputedData.Errors, err)
		}
	}
	result.PrecomputedData.DeliveryServiceStats = stats
	return result
}

// processStat and its subsidiary functions act as a State Machine, flowing the stat thru states for each "." component of the stat name
func processStat(server tc.CacheName, stats map[tc.DeliveryServiceName]dsdata.Stat, toData todata.TOData, stat string, value interface{}, timeReceived time.Time) (map[tc.DeliveryServiceName]dsdata.Stat, error) {
	parts := strings.Split(stat, ".")
	if len(parts) < 1 {
		return stats, fmt.Errorf("stat has no initial part")
	}

	switch parts[0] {
	case "plugin":
		return processStatPlugin(server, stats, toData, stat, parts[1:], value, timeReceived)
	case "proxy":
		return stats, dsdata.ErrNotProcessedStat
	case "server":
		return stats, dsdata.ErrNotProcessedStat
	default:
		return stats, fmt.Errorf("stat '%s' has unknown initial part '%s'", stat, parts[0])
	}
}

func processStatPlugin(server tc.CacheName, stats map[tc.DeliveryServiceName]dsdata.Stat, toData todata.TOData, stat string, statParts []string, value interface{}, timeReceived time.Time) (map[tc.DeliveryServiceName]dsdata.Stat, error) {
	if len(statParts) < 1 {
		return stats, fmt.Errorf("stat has no plugin part")
	}
	switch statParts[0] {
	case "remap_stats":
		return processStatPluginRemapStats(server, stats, toData, stat, statParts[1:], value, timeReceived)
	default:
		return stats, fmt.Errorf("stat has unknown plugin part '%s'", statParts[0])
	}
}

func processStatPluginRemapStats(server tc.CacheName, stats map[tc.DeliveryServiceName]dsdata.Stat, toData todata.TOData, stat string, statParts []string, value interface{}, timeReceived time.Time) (map[tc.DeliveryServiceName]dsdata.Stat, error) {
	if len(statParts) < 3 {
		return stats, fmt.Errorf("stat has no remap_stats deliveryservice and name parts")
	}

	// the FQDN is `subsubdomain`.`subdomain`.`domain`. For a HTTP delivery service, `subsubdomain` will be the cache hostname; for a DNS delivery service, it will be `edge`. Then, `subdomain` is the delivery service regex.
	subsubdomain := statParts[0]
	subdomain := statParts[1]
	domain := strings.Join(statParts[2:len(statParts)-1], ".")

	ds, ok := toData.DeliveryServiceRegexes.DeliveryService(domain, subdomain, subsubdomain)
	if !ok {
		fqdn := fmt.Sprintf("%s.%s.%s", subsubdomain, subdomain, domain)
		return stats, fmt.Errorf("ERROR no delivery service match for fqdn '%v' stat '%v'\n", fqdn, strings.Join(statParts, "."))
	}
	if ds == "" {
		fqdn := fmt.Sprintf("%s.%s.%s", subsubdomain, subdomain, domain)
		return stats, fmt.Errorf("ERROR EMPTY delivery service fqdn %v stat %v\n", fqdn, strings.Join(statParts, "."))
	}

	statName := statParts[len(statParts)-1]

	dsStat, ok := stats[ds]
	if !ok {
		newStat := dsdata.NewStat()
		dsStat = *newStat
	}

	if err := addCacheStat(&dsStat.TotalStats, statName, value); err != nil {
		return stats, err
	}

	cachegroup, ok := toData.ServerCachegroups[server]
	if !ok {
		return stats, fmt.Errorf("server missing from TOData.ServerCachegroups")
	}
	dsStat.CacheGroups[cachegroup] = dsStat.TotalStats

	cacheType, ok := toData.ServerTypes[server]
	if !ok {
		return stats, fmt.Errorf("server missing from TOData.ServerTypes")
	}
	dsStat.Types[cacheType] = dsStat.TotalStats

	dsStat.Caches[server] = dsStat.TotalStats

	dsStat.CachesTimeReceived[server] = timeReceived
	stats[ds] = dsStat
	return stats, nil
}

// addCacheStat adds the given stat to the existing stat. Note this adds, it doesn't overwrite. Numbers are summed, strings are concatenated.
// TODO make this less duplicate code somehow.
func addCacheStat(stat *dsdata.StatCacheStats, name string, val interface{}) error {
	switch name {
	case "status_2xx":
		v, ok := val.(float64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.Status2xx.Value += int64(v)
	case "status_3xx":
		v, ok := val.(float64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.Status3xx.Value += int64(v)
	case "status_4xx":
		v, ok := val.(float64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.Status4xx.Value += int64(v)
	case "status_5xx":
		v, ok := val.(float64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.Status5xx.Value += int64(v)
	case "out_bytes":
		v, ok := val.(float64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.OutBytes.Value += int64(v)
	case "is_available":
		v, ok := val.(bool)
		if !ok {
			return fmt.Errorf("stat '%s' value expected bool actual '%v' type %T", name, val, val)
		}
		if v {
			stat.IsAvailable.Value = true
		}
	case "in_bytes":
		v, ok := val.(float64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.InBytes.Value += v
	case "tps_2xx":
		v, ok := val.(int64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.Tps2xx.Value += float64(v)
	case "tps_3xx":
		v, ok := val.(int64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.Tps3xx.Value += float64(v)
	case "tps_4xx":
		v, ok := val.(int64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.Tps4xx.Value += float64(v)
	case "tps_5xx":
		v, ok := val.(int64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.Tps5xx.Value += float64(v)
	case "error_string":
		v, ok := val.(string)
		if !ok {
			return fmt.Errorf("stat '%s' value expected string actual '%v' type %T", name, val, val)
		}
		stat.ErrorString.Value += v + ", "
	case "tps_total":
		v, ok := val.(float64)
		if !ok {
			return fmt.Errorf("stat '%s' value expected int actual '%v' type %T", name, val, val)
		}
		stat.TpsTotal.Value += v
	case "status_unknown":
		return dsdata.ErrNotProcessedStat
	default:
		return fmt.Errorf("unknown stat '%s'", name)
	}
	return nil
}
