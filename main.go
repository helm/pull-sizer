/*
Copyright The Helm Authors.
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

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
	gin "gopkg.in/gin-gonic/gin.v1"
)

var (
	// The shared secret between the app and GitHub
	// This application only works with one shared secret. It was designed for
	// a single repo or org situation. Or a place where one org can safely use
	// the same secret.
	// Note, feature requests to make this work with multiple shared secrets welcome
	sharedSecret string

	// The name of the repo or org this should process events for. Again, feature
	// requests welcome to enable multi-org and multi-repo situations.
	repoOrOrgName string

	// The token for the user/bot that will be updating the label and sending
	// a notification
	ghToken string
)

// Sizes contains a map where the keys are the labels. The first size found in
// range is the one that will be used.
// TODO: Add a check to make sure sizes do not overlap
type Sizes map[string]Size

// Size contains the details on an individual size
type Size struct {
	Min int64
	Max int64
}

// TODO: Provide means to pass in or otherwise change the default sizes and labels
var defaultSizes Sizes

func init() {
	defaultSizes = Sizes{
		"size/XS": Size{
			Min: 0,
			Max: 9,
		},
		"size/S": Size{
			Min: 10,
			Max: 29,
		},
		"size/M": Size{
			Min: 30,
			Max: 99,
		},
		"size/L": Size{
			Min: 100,
			Max: 499,
		},
		"size/XL": Size{
			Min: 500,
			Max: 999,
		},
		"size/XXL": Size{
			Min: 1000,
			Max: math.MaxInt64,
		},
	}
}

func main() {

	// Get config from environment
	sharedSecret = os.Getenv("GITHUB_SHARED_SECRET")
	repoOrOrgName = os.Getenv("GITHUB_REPO_NAME")
	ghToken = os.Getenv("GITHUB_TOKEN")

	// Disable color in output
	gin.DisableConsoleColor()

	router := gin.New()

	// Recovery enables Gin to handle panics and provides a 500 error
	router.Use(gin.Recovery())

	// gin.Default() setups up recovery and logging on all paths. In this case
	// we want to skip /healthz checks so as not to clutter up the logs.
	router.Use(gin.LoggerWithWriter(gin.DefaultWriter, "/healthz"))

	// We can use this to check the health or and make sure the app is online
	router.GET("/healthz", healthz)

	router.POST("/webhook", processHook)

	router.Run()
}

func healthz(c *gin.Context) {
	c.String(http.StatusOK, http.StatusText(http.StatusOK))
}

func processHook(c *gin.Context) {

	// Validate payload
	sig := c.GetHeader("X-Hub-Signature")
	if sig == "" {
		c.JSON(http.StatusBadRequest, gin.H{"message": "Missing X-Hub-Signature"})
		return
	}

	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		logit("ERROR: Failed to read request body: %s", err)
		c.JSON(http.StatusBadRequest, gin.H{"message": "Malformed body"})
		return
	}
	defer c.Request.Body.Close()

	if err := validateSig(sig, body); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"message": "Validating payload against signature failed"})
		return
	}

	// Check the event type. Make sure just a PR
	// We need to get the event from the Request object as Gin in the middle
	// does some normalization that breaks this particular header name.
	event := c.Request.Header.Get("X-GitHub-Event")

	// When pull requests come in we check them. Pull requests tell
	// us when code has changed to check. Comments allow us to re-trigger a check
	// for situations when the bot goes offline or some other problem occurs.
	if event != "pull_request" {
		c.JSON(http.StatusOK, gin.H{"message": "Skipping event type"})
		return
	}

	// Get the payload body as an object
	e, err := github.ParseWebHook(event, body)
	if err != nil {
		logit("ERROR: Failed to parse body: %s", err)
		c.JSON(http.StatusBadRequest, gin.H{"message": "Malformed body"})
		return
	}

	payload := e.(*github.PullRequestEvent)

	// Check this is a repo we operate on
	repoSplit := strings.Split(*payload.Repo.FullName, "/")
	checkSplit := strings.Split(repoOrOrgName, "/")

	if repoSplit[0] != checkSplit[0] {
		c.JSON(http.StatusForbidden, gin.H{"message": "Not configured for this repository"})
		return
	} else if len(checkSplit) > 1 && repoSplit[1] != checkSplit[1] {
		c.JSON(http.StatusForbidden, gin.H{"message": "Not configured for this repository"})
		return
	}

	// Filter pull request actions we aren't intersted in like labels being added/removed
	if *payload.Action != "opened" && *payload.Action != "synchronize" && *payload.Action != "reopened" {
		c.JSON(http.StatusOK, gin.H{"message": "Skipping action"})
		return
	}

	// Get changes
	changes, err := readPaginatedFileChanges(repoSplit[0], repoSplit[1], *payload.Number)
	if err != nil {
		logit("ERROR: Failed to get PR chanages: %s", err)
		c.JSON(http.StatusBadRequest, gin.H{"message": "Error reading PR changes"})
		return
	}

	// Post label
	label, err := updateLabel(repoSplit[0], repoSplit[1], *payload.Number, changes)
	if err != nil {
		logit("ERROR: Unable to add label %s to %s/%s number %d: %s", label, repoSplit[0], repoSplit[1], *payload.Number, err)
		c.JSON(http.StatusInternalServerError, gin.H{"message": "Error processing request (1)"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Success"})
}

func validateSig(sig string, body []byte) error {
	digest := hmac.New(sha1.New, []byte(sharedSecret))
	digest.Write(body)
	sum := digest.Sum(nil)
	checksum := fmt.Sprintf("sha1=%x", sum)
	if subtle.ConstantTimeCompare([]byte(checksum), []byte(sig)) != 1 {
		logit("ERROR: Expected signature %q, but got %q", checksum, sig)
		return errors.New("payload signature check failed")
	}
	return nil
}

func logit(message string, vars ...interface{}) {
	fmt.Fprintf(gin.DefaultWriter, "[APP] "+message+"\n", vars...)
}

func ghClient() (context.Context, *github.Client) {
	t := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: ghToken})
	c := context.Background()
	tc := oauth2.NewClient(c, t)
	return c, github.NewClient(tc)
}

// GitHub limits lists to 100 entries. After that you need to use paging. Here we
// get details from all files by using paging.
func readPaginatedFileChanges(owner, repo string, number int) (int, error) {

	ctx, client := ghClient()

	opts := &github.ListOptions{
		PerPage: 100,
	}

	changes := 0

	for {
		cfs, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode != 200 {
			return 0, fmt.Errorf("Unable to get changed files. Status code: %s", resp.Status)
		}

		for _, cf := range cfs {
			changes += *cf.Changes
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage

	}

	return changes, nil
}

// Remove old labels and add correct one
func updateLabel(owner, repo string, number, changes int) (string, error) {

	ctx, client := ghClient()

	// Remove old size labels
	for k := range defaultSizes {
		res, err := client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, k)
		if res.StatusCode == 404 {
			continue
		}
		if err != nil {
			return "", err
		}
	}

	// Add label
	for k, v := range defaultSizes {
		c64 := int64(changes)
		if c64 >= v.Min && c64 <= v.Max {
			labels := []string{k}

			// For this to work the bot account needs access to create labels
			_, _, err := client.Issues.AddLabelsToIssue(ctx, owner, repo, number, labels)

			// TODO: Handle case where Bot does not have access to create labels.

			if err != nil {
				return k, err
			}

			return k, nil
		}
	}

	return "", nil
}
