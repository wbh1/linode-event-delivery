package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/linode/linodego"
	"golang.org/x/oauth2"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	badger "github.com/dgraph-io/badger/v2"
)

type tomlConfig struct {
	DB      database
	Sink    sink
	Sources map[string]source
}

type database struct {
	Path string
}

type source struct {
	ID       string
	Type     string
	Token    string
	// TODO: handle time.Duration right
	Interval string
}

type sink struct {
	Type string
	// TODO: handle url right.
	URL string
}

var config tomlConfig

var db *badger.DB

type IngestService struct {
	DB     *badger.DB
	Config tomlConfig
}

// LinodeEvent represents a linodego.Event with additional metadata
type LinodeEvent struct {
	Source    string         `json:"source"`
	Event     linodego.Event `json:"event"`
	Timestamp time.Time      `json:"timestamp"`
}

// PopulateLinodeEvent is responsible for taking a linodego.Event and adding additional metadata
func populateLinodeEvent(event linodego.Event, source string) LinodeEvent {
	log.Print("consider it populated.")
	return LinodeEvent{
		Source:    source,
		Event:     event,
		Timestamp: time.Now(),
	}
}

func filterNewLinodeEvents(db *badger.DB, events []linodego.Event, sourceID string) []linodego.Event {
	var newEvents []linodego.Event

	for _, event := range events {
		if isEventNew(db, event, sourceID) {
			newEvents = append(newEvents, event)
		}
	}

	return newEvents
}

func isEventNew(db *badger.DB, event linodego.Event, sourceID string) bool {
	// TODO: make prefix configurable
	prefix := []byte(fmt.Sprintf("linode-account-event-%s-%s-", sourceID, event.Status))
	isNew := false

	err := db.View(func(txn *badger.Txn) error {
		_, err := txn.Get([]byte(fmt.Sprintf("%s-%d", prefix, event.ID)))
		if err != nil {
			if err != badger.ErrKeyNotFound {
				log.Fatal(err)
			}
			isNew = true
			return nil
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	return isNew
}

// TODO: stop passing around just the db
func markLinodeEventAsSent(db *badger.DB, event linodego.Event, sourceID string) {
	// TODO: make prefix configurable
	prefix := []byte(fmt.Sprintf("linode-account-event-%s-%s-", sourceID, event.Status))

	err := db.Update(func(txn *badger.Txn) error {
		_ = txn.Set([]byte(fmt.Sprintf("%s-%d", prefix, event.ID)), []byte(strconv.Itoa(1)))
		fmt.Sprintf("%s-%d", prefix, event.ID)

		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}

func forwardLinodeEvent(event LinodeEvent, sink sink) {
	conn, err := net.Dial("tcp", sink.URL)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// TODO: learn how to do errors right
	message, merr := json.Marshal(event)
	if merr != nil {
		log.Fatal(merr)
	}

	// send to socket
	fmt.Fprintf(conn, string(message)+"\n")
	log.Print(fmt.Sprintf("sent %s", message))
}

func createLinodeClient(config source) linodego.Client {
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.Token})

	oauth2Client := &http.Client{
		Transport: &oauth2.Transport{
			Source: tokenSource,
		},
	}

	client := linodego.NewClient(oauth2Client)
	//client.SetDebug(true)

	return client
}

func listNewLinodeEvents(db *badger.DB, linode linodego.Client, sourceID string) []linodego.Event {
	filter := fmt.Sprintf("{}")
	opts := linodego.NewListOptions(1, filter)

	allEvents, err := linode.ListEvents(context.Background(), opts)
	if err != nil {
		log.Fatal("Error getting Events, expected struct, got error %v", err)
	}

	filteredEvents := filterNewLinodeEvents(db, allEvents, sourceID)

	return filteredEvents
}

func (service IngestService) Start(source string, sourceConfig source) {
	client := createLinodeClient(sourceConfig)

	interval, err := time.ParseDuration(sourceConfig.Interval)
	if err != nil {
		log.Fatal(err)
	}

	firstRun := true

	c := time.Tick(interval)

	for range c {
		go func() {
			log.Print(fmt.Sprintf("checking for new events source=%s", source))
			events := listNewLinodeEvents(db, client, source)

			for _, event := range events {
				// add extra info
				// TODO: fix odd type change
				e := populateLinodeEvent(event, source)
				// send it
				if ! firstRun {
					forwardLinodeEvent(e, config.Sink)
				}
				// mark it as sent
				markLinodeEventAsSent(db, event, source)
			}

			firstRun = false
		}()
	}
}

func main() {
	// config
	if _, err := toml.DecodeFile("/etc/ingest/ingest.toml", &config); err != nil {
		log.Fatal(err)
	}

	// persistence
	// TODO: learn how to do errors right
	db, _ = badger.Open(badger.DefaultOptions(config.DB.Path))

	// service
	service := IngestService{
		DB:     db,
		Config: config,
	}

	var waitGroup sync.WaitGroup
	waitGroup.Add(len(config.Sources))
	for source, sourceConfig := range config.Sources {
		go service.Start(source, sourceConfig)
	}
	waitGroup.Wait()
}
