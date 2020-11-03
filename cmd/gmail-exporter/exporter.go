package main

import (
	"crypto/sha512"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	ghttp "github.com/glynternet/pkg/http"
	"github.com/glynternet/pkg/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

func buildCmdTree(logger log.Logger, _ *os.File, rootCmd *cobra.Command) {
	rootCmd.RunE = run(logger)
}

var refreshLabelsErrs = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "gmail",
	Name:      "refresh_labels_errors_total",
	Help:      "Number of refresh labels errors.",
})

// Retrieve a token, saves the token, then returns the generated client.
func getClient(config *oauth2.Config) (*http.Client, error) {
	// The file token.json stores the user's access and refresh tokens, and is
	// created automatically when the authorization flow completes for the first
	// time.
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok, _ = getTokenFromWeb(config)
		if err := saveToken(tokFile, tok); err != nil {
			return nil, errors.Wrap(err, "saving token")
		}
	}
	return config.Client(context.Background(), tok), nil
}

// Request a token from the web, then returns the retrieved token.
func getTokenFromWeb(config *oauth2.Config) (*oauth2.Token, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		return nil, fmt.Errorf("unable to read authorization code: %v", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve token from web: %v", err)
	}
	return tok, nil
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
func saveToken(path string, token *oauth2.Token) error {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
	return nil
}

func run(logger log.Logger) func(*cobra.Command, []string) error {
	return func(command *cobra.Command, strings []string) error {
		b, err := ioutil.ReadFile("credentials.json")
		if err != nil {
			return fmt.Errorf("unable to read client secret file: %v", err)
		}

		scrapeToken, err := ioutil.ReadFile("scrape_token")
		if err != nil {
			return fmt.Errorf("unable to read scrape token file: %v", err)
		}

		// If modifying these scopes, delete your previously saved token.json.
		config, err := google.ConfigFromJSON(b, gmail.GmailMetadataScope)
		if err != nil {
			return fmt.Errorf("unable to parse client secret file to config: %v", err)
		}

		client, err := getClient(config)
		if err != nil {
			return errors.Wrap(err, "getting client")
		}

		srv, err := gmail.New(client)
		if err != nil {
			return fmt.Errorf("unable to retrieve Gmail client: %v", err)
		}

		e := exporter{
			UsersLabelsService: srv.Users.Labels,
			labelRefreshPeriod: time.Minute * 5,
			logger:             logger,
		}
		go e.startRefreshingLabels()

		prometheus.NewPedanticRegistry()

		// Expose the registered metrics via HTTP.
		registry := prometheus.NewRegistry()
		if err = registry.Register(&e); err != nil {
			panic(err)
		}

		if err = registry.Register(refreshLabelsErrs); err != nil {
			panic(err)
		}

		http.Handle("/metrics", ghttp.WithAuthoriser(logger, newBearerTokenAuthoriser(scrapeToken), promhttp.HandlerFor(
			registry,
			promhttp.HandlerOpts{
				// Opt into OpenMetrics to support exemplars.
				EnableOpenMetrics: true,
			},
		)))

		addr := ":8765"
		fmt.Println("listening at ", addr)
		return errors.Wrap(http.ListenAndServe(addr, nil), "serving")
	}
}

func newBearerTokenAuthoriser(token []byte) bearerTokenAuthoriser {
	return bearerTokenAuthoriser{authorizedHeaderSha512: sha512.Sum512(append([]byte("Bearer "), token...))}
}

type bearerTokenAuthoriser struct {
	authorizedHeaderSha512 [64]byte
}

func (b bearerTokenAuthoriser) Authorise(r *http.Request) error {
	authHeader := r.Header.Get("Authorization")
	givenSha := sha512.Sum512([]byte(authHeader))
	if subtle.ConstantTimeCompare(givenSha[:], b.authorizedHeaderSha512[:]) != 1 {
		return errors.New("invalid authorization header")
	}
	return nil
}

type exporter struct {
	*gmail.UsersLabelsService
	labelRefreshPeriod time.Duration
	refreshingLabels
	logger log.Logger
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
			e.logger.Log(log.Error(fmt.Errorf("getting label data for label: %s: %v", label.id, err)))
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

func (e *exporter) startRefreshingLabels() {
	if e.refreshingLabels == nil {
		e.refreshingLabels = refreshingLabels{}
	}

	if err := e.refreshLabels(); err != nil {
		_ = e.logger.Log(
			log.Message("Error doing initial periodic label refresh"),
			log.Error(err))
	}
	fmt.Println("Labels: ", e.refreshingLabels)
	// TODO(gynternet): handle stopping ticker
	t := time.NewTicker(e.labelRefreshPeriod)

	for range t.C {
		if err := e.refreshLabels(); err != nil {
			_ = e.logger.Log(
				log.Message("Error doing periodic label refresh"),
				log.Error(err))
		}
		// log here
		_ = e.logger.Log(log.Message("Labels refreshed"), log.KV{
			K: "labels",
			V: e.refreshingLabels,
		})
	}
}

const userID = "me"

func (e *exporter) refreshLabels() error {
	err := e.refresh(e.List(userID).Do)
	if err != nil {
		refreshLabelsErrs.Inc()
	}
	return err
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

func (rls *refreshingLabels) refresh(get labelsGetter) error {
	// only id, name, messageListVisibility, labelListVisibility, and type are returned by a labels list call
	// https://developers.google.com/gmail/api/v1/reference/users/labels/list
	r, err := get()
	if err != nil {
		return errors.Wrap(err, "getting labels")
	}
	if len(r.Labels) == 0 {
		*rls = nil
		// TODO: WARN log
		fmt.Println("No labels found.")
		return nil
	}
	ls := make([]gmailLabel, len(r.Labels))
	for i, l := range r.Labels {
		ls[i] = gmailLabel{name: l.Name, id: l.Id, ttype: l.Type}
	}
	*rls = ls
	return nil
}

type labelsGetter func(opts ...googleapi.CallOption) (*gmail.ListLabelsResponse, error)
