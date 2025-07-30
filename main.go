package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
	"go.oneofone.dev/gserv"
)

const cacheDuration = 500 * time.Millisecond

var cacheMutex sync.Mutex
var cachedResponse gserv.Response
var lastCacheTime time.Time

type oidPrefix string

const (
	AttenuationDb           oidPrefix = ".1.3.6.1.2.1.10.94.1.1.2.1.5"
	OutputPowerDbm          oidPrefix = ".1.3.6.1.2.1.10.94.1.1.2.1.7"
	CurrentSyncRateBps      oidPrefix = ".1.3.6.1.2.1.10.251.1.2.2.1.2"
	MaxSyncRateBps          oidPrefix = ".1.3.6.1.2.1.10.94.1.1.2.1.8"
	SnrMarginDb             oidPrefix = ".1.3.6.1.2.1.10.94.1.1.2.1.4"
	InterleaveDepth         oidPrefix = ".1.3.6.1.2.1.10.251.1.2.2.1.10"
	InterleaveDelayMs       oidPrefix = ".1.3.6.1.2.1.10.251.1.2.2.1.4"
	ActualImpulseProtection oidPrefix = ".1.3.6.1.2.1.10.251.1.2.2.1.5"
	Fecs                    oidPrefix = ".1.3.6.1.2.1.10.251.1.2.2.1.7"
	IpAddressIfIndex        oidPrefix = ".1.3.6.1.2.1.4.20.1.2"
	DownstreamDslStatus     oidPrefix = ".1.3.6.1.2.1.10.94.1.1.2.1.6"
)

type oidMetadata struct {
	oidPrefix        oidPrefix
	description      string
	unit             string
	fullOidTemplates []string
	valueFormatter   func(interface{}) string
}

func (o oidMetadata) withCustomOidTemplates(templates ...string) oidMetadata {
	o.fullOidTemplates = templates
	return o
}

func describeIntegerOid(prefix oidPrefix, description string, isDirectional bool, unit string) oidMetadata {
	return describeFormattedIntegerOid(prefix, description, isDirectional, unit, func(i uint) string {
		return fmt.Sprintf("%d", i)
	})
}

func describeFormattedIntegerOid(prefix oidPrefix, description string, isDirectional bool, unit string, valueFormatter func(uint) string) oidMetadata {
	compositeTransformer := func(rawValue interface{}) string {
		integerValue, castOk := rawValue.(uint)
		if !castOk {
			signedIntegerValue, castOk := rawValue.(int)
			if castOk {
				integerValue = uint(signedIntegerValue)
			} else {
				return fmt.Sprintf("(wrong type: %T)", rawValue)
			}
		}

		return valueFormatter(integerValue)
	}

	var fullOidTemplates []string
	if isDirectional {
		fullOidTemplates = []string{
			"{Prefix}.{IfIndex}.{DownstreamUnitId}",
			"{Prefix}.{IfIndex}.{UpstreamUnitId}",
		}
	} else {
		fullOidTemplates = []string{
			"{Prefix}.{IfIndex}",
		}
	}

	return oidMetadata{
		oidPrefix:        prefix,
		description:      description,
		fullOidTemplates: fullOidTemplates,
		unit:             unit,
		valueFormatter:   compositeTransformer,
	}
}

var oidMetadataList = []oidMetadata{
	{DownstreamDslStatus, "Sync status", "", []string{fmt.Sprintf("%s.{IfIndex}", DownstreamDslStatus)}, func(i interface{}) string {
		value, castOk := i.([]uint8)
		if !castOk {
			return fmt.Sprintf("(wrong type: %T)", i)
		}

		return string(value)
	}},
	describeIntegerOid(AttenuationDb, "Attenuation (down/up)", true, "dB").withCustomOidTemplates(
		".1.3.6.1.2.1.10.94.1.1.2.1.5.{IfIndex}",
		".1.3.6.1.2.1.10.94.1.1.3.1.5.{IfIndex}"),
	describeIntegerOid(OutputPowerDbm, "Output power (down/up)", true, "dBm").withCustomOidTemplates(
		".1.3.6.1.2.1.10.94.1.1.2.1.7.{IfIndex}",
		".1.3.6.1.2.1.10.94.1.1.3.1.7.{IfIndex}"),
	describeFormattedIntegerOid(CurrentSyncRateBps, "Current rate (down/up)", true, "Kbps", func(i uint) string {
		return fmt.Sprintf("%d", i/1000)
	}),
	describeFormattedIntegerOid(MaxSyncRateBps, "Max rate (down/up)", true, "Kbps", func(i uint) string {
		return fmt.Sprintf("%d", i/1000)
	}).withCustomOidTemplates(
		".1.3.6.1.2.1.10.94.1.1.2.1.8.{IfIndex}",
		".1.3.6.1.2.1.10.94.1.1.3.1.8.{IfIndex}"),
	describeIntegerOid(SnrMarginDb, "SNR margin (down/up)", true, "dB").withCustomOidTemplates(
		".1.3.6.1.2.1.10.94.1.1.2.1.4.{IfIndex}",
		".1.3.6.1.2.1.10.94.1.1.3.1.4.{IfIndex}"),
	describeFormattedIntegerOid(InterleaveDepth, "Interleave depth (down/up)", true, "", func(i uint) string {
		if i == 1 {
			return "Fast (1)"
		}

		return fmt.Sprintf("Interleaved (%d)", i)
	}),
	describeIntegerOid(InterleaveDelayMs, "Interleave delay (down/up)", true, "ms"),
	describeIntegerOid(ActualImpulseProtection, "Impulse Protection (down/up)", true, "units"),
	describeIntegerOid(Fecs, "FECS (down/up)", true, ""),
}

const ifTypeMibPrefix = ".1.3.6.1.2.1.2.2.1.3"
const vdsl2ChannelType = 251
const terminationUnitOidPrefix = ".1.3.6.1.2.1.10.251.1.2.2.1.1"
const upstreamTerminationUnit = 1
const downstreamTerminationUnit = 2

var (
	port      int
	snmpIP    string
	snmpPort  int
	community string
)

func main() {
	flag.IntVar(&port, "p", 8080, "HTTP port")
	flag.StringVar(&snmpIP, "ip", "127.0.0.1", "SNMP IP address")
	flag.IntVar(&snmpPort, "port", 161, "SNMP port (default: 161)")
	flag.StringVar(&community, "community", "public", "SNMP community name")

	flag.Parse()

	if port > 65535 || port <= 0 {
		panic("Invalid HTTP port")
	}

	start(port)
}

func start(port int) {
	srv := gserv.New()
	svc := &Svc{
		snmpClient: setupSnmp(),
	}
	srv.GET("/", CreateCacheHandler(svc.HandleRequest))

	fmt.Printf("Listening on port %d. Press CTRL+C to exit...\n", port)
	log.Panic(srv.Run(context.Background(), "0.0.0.0:"+fmt.Sprintf("%d", port)))
}

type Svc struct {
	snmpClient *gosnmp.GoSNMP
}

func setupSnmp() *gosnmp.GoSNMP {
	client := &gosnmp.GoSNMP{
		Target:    snmpIP,
		Port:      uint16(snmpPort),
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   time.Second * 5,
	}
	err := client.Connect()
	if err != nil {
		log.Fatalf("Failed to connect via SNMP: %v", err)
	}

	return client
}

func findVdslIfIndex(client *gosnmp.GoSNMP) string {
	ifTypes, err := client.BulkWalkAll(ifTypeMibPrefix)
	if err != nil {
		log.Fatalf("Failed to bulk walk ifTypes MIB: %v", err)
	}

	for _, ifType := range ifTypes {
		value, castOk := ifType.Value.(int)

		if castOk && value == vdsl2ChannelType {
			parts := strings.Split(ifType.Name, ".")
			if len(parts) > 0 {
				return parts[len(parts)-1]
			}
		}
	}

	log.Fatalf("Failed to find vdsl2 if index from snmp")
	return ""
}

func findTerminationUnitIds(client *gosnmp.GoSNMP, vdslIfIndex string) (upstreamOidSuffix string, downstreamOidSuffix string) {
	upstreamOid := fmt.Sprintf(
		"%s.%s.%d", terminationUnitOidPrefix, vdslIfIndex, upstreamTerminationUnit)

	downstreamOid := fmt.Sprintf(
		"%s.%s.%d", terminationUnitOidPrefix, vdslIfIndex, downstreamTerminationUnit)

	results, err := client.Get([]string{upstreamOid, downstreamOid})
	if err != nil {
		log.Fatalf("Failed to get downstream/upstream direction MIBs: %v", err)
	}

	for _, variable := range results.Variables {
		value, castOk := variable.Value.(int)
		if !castOk {
			log.Fatalf("Failed to get downstream/upstream direction MIBs. Unexpected type")
			return upstreamOidSuffix, downstreamOidSuffix
		}

		if variable.Name == upstreamOid {
			upstreamOidSuffix = fmt.Sprintf("%d", value)
		}

		if variable.Name == downstreamOid {
			downstreamOidSuffix = fmt.Sprintf("%d", value)
		}
	}

	return upstreamOidSuffix, downstreamOidSuffix
}

func (s *Svc) HandleRequest(*gserv.Context) gserv.Response {
	var html bytes.Buffer

	html.WriteString("<!DOCTYPE html>")

	//goland:noinspection SpellCheckingInspection
	html.WriteString(`<html><head>
  <meta http-equiv="refresh" content="1">
  <title>VDSL Statistics</title></head><body><dl>`)

	// Helper to add dt/dd entries
	addEntry := func(dt, dd string) {
		_, err := fmt.Fprintf(&html, "<dt>%s</dt><dd>%s</dd>", dt, dd)
		if err != nil {
			panic("Failed to append buffer")
		}
	}

	vdslIfIndex := findVdslIfIndex(s.snmpClient)
	xtucUpstreamSubId, xturDownstreamSubId := findTerminationUnitIds(s.snmpClient, vdslIfIndex)
	ipAddress := findVdslPppAdress(s.snmpClient, vdslIfIndex)
	addEntry("PPP IP Address", ipAddress)

	fullOidsByOidPrefix := make(map[oidPrefix][]string)
	valuesByQueryOids := make(map[string]interface{})
	var queryOids []string

	for _, item := range oidMetadataList {
		var currentItemFullOids []string

		for _, fullOidTemplate := range item.fullOidTemplates {
			var fullOid = strings.Replace(fullOidTemplate, "{Prefix}", string(item.oidPrefix), 1)
			fullOid = strings.Replace(fullOid, "{IfIndex}", vdslIfIndex, 1)
			fullOid = strings.Replace(fullOid, "{DownstreamUnitId}", xturDownstreamSubId, 1)
			fullOid = strings.Replace(fullOid, "{UpstreamUnitId}", xtucUpstreamSubId, 1)
			valuesByQueryOids[fullOid] = ""
			queryOids = append(queryOids, fullOid)
			currentItemFullOids = append(currentItemFullOids, fullOid)
		}

		fullOidsByOidPrefix[item.oidPrefix] = currentItemFullOids
	}

	result, err := s.snmpClient.Get(queryOids)
	if err != nil {
		log.Printf("Error fetching all OIDs: %v", err)
		addEntry("Status", "SNMP Error")
	} else {
		for _, v := range result.Variables {
			valuesByQueryOids[v.Name] = v.Value
		}
	}

	for _, item := range oidMetadataList {
		expectedFullOids := fullOidsByOidPrefix[item.oidPrefix]
		if len(expectedFullOids) == 2 {
			addEntry(
				item.description,
				fmt.Sprintf(
					"%s / %s %s",
					item.valueFormatter(valuesByQueryOids[expectedFullOids[0]]),
					item.valueFormatter(valuesByQueryOids[expectedFullOids[1]]),
					item.unit))
		} else if len(expectedFullOids) == 1 {
			addEntry(
				item.description,
				fmt.Sprintf(
					"%s %s",
					item.valueFormatter(valuesByQueryOids[expectedFullOids[0]]),
					item.unit))
		} else {
			addEntry(item.description, "(error: unexpected oid count)")
		}
	}

	html.WriteString("</dl></body></html>")

	return gserv.PlainResponse("text/html", html.String())
}

func findVdslPppAdress(client *gosnmp.GoSNMP, vdslIfIndex string) string {
	result, err := client.WalkAll(string(IpAddressIfIndex))
	if err != nil {
		return fmt.Sprintf("(error: %v)", err)
	}

	for _, result := range result {
		value, castOk := result.Value.(int)
		if !castOk {
			continue
		}

		foundIfIndex := fmt.Sprintf("%d", value)
		if foundIfIndex == vdslIfIndex {
			ipAddress := strings.TrimPrefix(result.Name, fmt.Sprintf("%s.", string(IpAddressIfIndex)))
			return ipAddress
		}
	}

	return fmt.Sprintf("(not found)")
}

func CreateCacheHandler(handler func(*gserv.Context) gserv.Response) func(*gserv.Context) gserv.Response {
	return func(ctx *gserv.Context) gserv.Response {
		cacheMutex.Lock()
		defer cacheMutex.Unlock()

		if time.Since(lastCacheTime) < cacheDuration && cachedResponse != nil {
			return cachedResponse
		}

		newResponse := handler(ctx)
		cachedResponse = newResponse
		lastCacheTime = time.Now()

		return newResponse
	}
}
