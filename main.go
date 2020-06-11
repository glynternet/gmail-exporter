/**
 * @license
 * Copyright Google Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
// [START gmail_quickstart]
package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

// Retrieve a token, saves the token, then returns the generated client.
func getClient(config *oauth2.Config) *http.Client {
	// The file token.json stores the user's access and refresh tokens, and is
	// created automatically when the authorization flow completes for the first
	// time.
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

// Request a token from the web, then returns the retrieved token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Fatalf("Unable to read authorization code: %v", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web: %v", err)
	}
	return tok
}

// Retrieves a token from a local file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// Saves a token to a file path.
func saveToken(path string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func main() {
	b, err := ioutil.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}

	// If modifying these scopes, delete your previously saved token.json.
	config, err := google.ConfigFromJSON(b, gmail.GmailMetadataScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	client := getClient(config)

	srv, err := gmail.New(client)
	if err != nil {
		log.Fatalf("Unable to retrieve Gmail client: %v", err)
	}

	e := exporter{
		UsersLabelsService: srv.Users.Labels,
		labelRefreshPeriod: time.Minute * 5,
	}
	go e.start()

	prometheus.NewPedanticRegistry()

	// Expose the registered metrics via HTTP.
	registry := prometheus.NewRegistry()
	if err = registry.Register(&e); err != nil {
		panic(err)
	}

	http.Handle("/metrics", promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{
			// Opt into OpenMetrics to support exemplars.
			EnableOpenMetrics: true,
		},
	))

	addr := ":8765"
	fmt.Println("listening at ", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

type exporter struct {
	*gmail.UsersLabelsService
	labelRefreshPeriod time.Duration
	refreshingLabels
}

func (e exporter) Describe(_ chan<- *prometheus.Desc) {
	// is this actually what I'm meant to do?
	// no descriptions if the labels may change over time?
	return
}

// TODO: instrument request time per label?

func (e *exporter) Collect(metrics chan<- prometheus.Metric) {
	const metricsPerLabel = 2
	metricsCount := len(e.refreshingLabels) * metricsPerLabel
	ms := make(chan prometheus.Metric, metricsCount)
	for _, label := range e.refreshingLabels {
		lData, err := e.Get(userID, label.id).Do()
		if err != nil {
			// how do I return an error so that it will be shown in prometheus here?
			log.Println(fmt.Errorf("getting label data for label: %s: %v", label.id, err))
			return
		}
		total, err := prometheus.NewConstMetric(
			prometheus.NewDesc(
				"gmail_messages_total",
				"total messages for a gmail label",
				nil,
				label.promLabels()),
			prometheus.GaugeValue,
			float64(lData.MessagesTotal))
		if err != nil {
			panic(fmt.Errorf("generating gmail_messages_total total: %s: %v", label.id, err))
		}
		ms <- total
		unread, err := prometheus.NewConstMetric(
			prometheus.NewDesc(
				"gmail_messages_unread_total",
				"unread messages for a gmail label",
				nil,
				label.promLabels()),
			prometheus.GaugeValue,
			float64(lData.MessagesUnread))
		if err != nil {
			panic(fmt.Errorf("generating gmail_messages_unread total: %s: %v", label.id, err))
		}
		ms <- unread
	}
	for len(ms) > 0 {
		metrics <- <-ms
	}
}

func (e *exporter) start() {
	if e.refreshingLabels == nil {
		e.refreshingLabels = refreshingLabels{}
	}

	e.refreshLabels()
	fmt.Println(e.refreshingLabels)
	// TODO(gynternet): handle stopping ticker
	t := time.NewTicker(e.labelRefreshPeriod)

	for range t.C {
		e.refreshLabels()
		// log here
		fmt.Println(e.refreshingLabels)
	}
}

const userID = "me"

func (e *exporter) refreshLabels() {
	e.refresh(e.List(userID).Do)
}

type refreshingLabels []gmailLabel

type gmailLabel struct {
	name, id, ttype string
}

func (l gmailLabel) promLabels() prometheus.Labels {
	return prometheus.Labels{
		"label_name": l.name,
		"label_id":   l.id,
		"label_type": l.ttype,
	}
}

func (rls *refreshingLabels) refresh(get labelsGetter) {
	// only id, name, messageListVisibility, labelListVisibility, and type are returned by a labels list call
	// https://developers.google.com/gmail/api/v1/reference/users/labels/list
	r, err := get()
	if err != nil {
		// TODO: handle
		log.Fatalf("Unable to retrieve labels: %v", err)
	}
	if len(r.Labels) == 0 {
		*rls = nil
		fmt.Println("No labels found.")
		return
	}
	ls := make([]gmailLabel, len(r.Labels))
	for i, l := range r.Labels {
		ls[i] = gmailLabel{name: l.Name, id: l.Id, ttype: l.Type}
	}
	*rls = ls
}

type labelsGetter func(opts ...googleapi.CallOption) (*gmail.ListLabelsResponse, error)

// [END gmail_quickstart]
