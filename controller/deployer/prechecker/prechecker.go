// Package prechecker checks that all the Cloud Foundry instances are running before a deploy.
package prechecker

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/compozed/deployadactyl/config"
	I "github.com/compozed/deployadactyl/interfaces"
	S "github.com/compozed/deployadactyl/structs"
	"github.com/go-errors/errors"
)

const (
	apiPathExtension        = "/v2/info"
	unavailableFoundation   = "deploy aborted, one or more CF foundations unavailable"
	noFoundationsConfigured = "no foundations configured"
	anAPIEndpointFailed     = "An api endpoint failed"
)

// Prechecker has an eventmanager used to manage event if prechecks fail.
type Prechecker struct {
	EventManager I.EventManager
}

// AssertAllFoundationsUp will send a request to each Cloud Foundry instance and check that the response status code is 200 OK.
func (p Prechecker) AssertAllFoundationsUp(environment config.Environment) error {
	var insecureClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			ResponseHeaderTimeout: 15 * time.Second,
		},
	}

	precheckerEventData := S.PrecheckerEventData{
		Environment: environment,
	}

	if len(environment.Foundations) == 0 {
		precheckerEventData.Description = noFoundationsConfigured

		p.EventManager.Emit(S.Event{Type: "validate.foundationsUnavailable", Data: precheckerEventData})
		return errors.Errorf(noFoundationsConfigured)
	}

	for _, foundationURL := range environment.Foundations {

		resp, err := insecureClient.Get(foundationURL + apiPathExtension)

		if err != nil {
			return errors.Errorf(unavailableFoundation)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			precheckerEventData.Description = unavailableFoundation

			p.EventManager.Emit(S.Event{Type: "validate.foundationsUnavailable", Data: precheckerEventData})
			return errors.Errorf("%s: %s: %s", anAPIEndpointFailed, foundationURL, resp.Status)
		}
	}
	return nil
}
