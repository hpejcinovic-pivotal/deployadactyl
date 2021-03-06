// Package deployer will deploy your application.
package deployer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/compozed/deployadactyl/config"
	"github.com/compozed/deployadactyl/controller/deployer/manifestro"
	"github.com/compozed/deployadactyl/geterrors"
	I "github.com/compozed/deployadactyl/interfaces"
	S "github.com/compozed/deployadactyl/structs"
	"github.com/go-errors/errors"
	"github.com/op/go-logging"
	"github.com/spf13/afero"
)

const (
	successfulDeploy = `Your deploy was successful! (^_^)b
If you experience any problems after this point, check that you can manually push your application to Cloud Foundry on a lower environment.
It is likely that it is an error with your application and not with Deployadactyl.
Thanks for using Deployadactyl! Please push down pull up on your lap bar and exit to your left.`

	deploymentOutput = `Deployment Parameters:
	Artifact URL: %s,
	Username:     %s,
	Environment:  %s,
	Org:          %s,
	Space:        %s,
	AppName:      %s`
)

// Deployer contains the bluegreener for deployments, environment variables, a fetcher for artifacts, a prechecker and event manager.
type Deployer struct {
	Config       config.Config
	BlueGreener  I.BlueGreener
	Fetcher      I.Fetcher
	Prechecker   I.Prechecker
	EventManager I.EventManager
	Randomizer   I.Randomizer
	Log          *logging.Logger
	FileSystem   *afero.Afero
}

// Deploy takes the deployment information, checks the foundations, fetches the artifact and deploys the application.
func (d Deployer) Deploy(req *http.Request, environment, org, space, appName, contentType string, out io.Writer) (statusCode int, err error) {
	var (
		deploymentInfo         = S.DeploymentInfo{}
		environments           = d.Config.Environments
		authenticationRequired = environments[environment].Authenticate
		deployEventData        = S.DeployEventData{}
		manifest               []byte
		appPath                string
	)
	defer d.FileSystem.RemoveAll(appPath)

	d.Log.Debug("prechecking the foundations")
	err = d.Prechecker.AssertAllFoundationsUp(environments[environment])
	if err != nil {
		fmt.Fprintln(out, err)
		return http.StatusInternalServerError, errors.New(err)
	}

	d.Log.Debug("checking for basic auth")
	username, password, ok := req.BasicAuth()
	if !ok {
		if authenticationRequired {
			return http.StatusUnauthorized, errors.New("basic auth header not found")
		}
		username = d.Config.Username
		password = d.Config.Password
	}

	if isJSON(contentType) {
		d.Log.Debug("deploying from json request")
		d.Log.Debug("building deploymentInfo")
		deploymentInfo, err = getDeploymentInfo(req.Body)
		if err != nil {
			fmt.Fprintln(out, err)
			return http.StatusInternalServerError, err
		}

		if deploymentInfo.Manifest != "" {
			manifest, err = base64.StdEncoding.DecodeString(deploymentInfo.Manifest)
			if err != nil {
				fmt.Fprintln(out, err)
				return http.StatusBadRequest, errors.New("cannot open manifest file")
			}
		}

		appPath, err = d.Fetcher.Fetch(deploymentInfo.ArtifactURL, deploymentInfo.Manifest)
		if err != nil {
			fmt.Fprintln(out, err)
			return http.StatusInternalServerError, err
		}

	} else if isZip(contentType) {
		d.Log.Debug("deploying from zip request")
		appPath, err = d.Fetcher.FetchZipFromRequest(req)
		if err != nil {
			return http.StatusInternalServerError, err
		}

		manifest, _ = d.FileSystem.ReadFile(appPath + "/manifest.yml")

		deploymentInfo.ArtifactURL = appPath
	} else {
		return http.StatusBadRequest, errors.New("must be application/json or application/zip")
	}

	deploymentInfo.Username = username
	deploymentInfo.Password = password
	deploymentInfo.Environment = environment
	deploymentInfo.Org = org
	deploymentInfo.Space = space
	deploymentInfo.AppName = appName
	deploymentInfo.UUID = d.Randomizer.StringRunes(128)
	deploymentInfo.SkipSSL = environments[environment].SkipSSL
	deploymentInfo.Manifest = string(manifest)

	instances := manifestro.GetInstances(deploymentInfo.Manifest)
	if instances != nil {
		deploymentInfo.Instances = *instances
	} else {
		deploymentInfo.Instances = environments[environment].Instances
	}

	e, found := environments[deploymentInfo.Environment]
	if !found {
		err = d.EventManager.Emit(S.Event{Type: "deploy.error", Data: deployEventData})
		if err != nil {
			fmt.Fprintln(out, err)
		}

		err = errors.Errorf("environment not found: %s", deploymentInfo.Environment)
		fmt.Fprintln(out, err)
		return http.StatusInternalServerError, err
	}

	deploymentMessage := fmt.Sprintf(deploymentOutput, deploymentInfo.ArtifactURL, deploymentInfo.Username, deploymentInfo.Environment, deploymentInfo.Org, deploymentInfo.Space, deploymentInfo.AppName)
	d.Log.Info(deploymentMessage)
	fmt.Fprintln(out, deploymentMessage)

	deployEventData = S.DeployEventData{Writer: out, DeploymentInfo: &deploymentInfo, RequestBody: req.Body}

	defer emitDeployFinish(d, deployEventData, out, &err, &statusCode)

	d.Log.Debug("emitting a deploy.start event")
	err = d.EventManager.Emit(S.Event{Type: "deploy.start", Data: deployEventData})
	if err != nil {
		fmt.Fprintln(out, err)
		return http.StatusInternalServerError, errors.Errorf("an error occurred in the deploy.start event: %s", err)
	}

	defer emitDeploySuccess(d, deployEventData, out, &err, &statusCode)

	err = d.BlueGreener.Push(e, appPath, deploymentInfo, out)
	if err != nil {
		if matched, _ := regexp.MatchString("login failed", err.Error()); matched {
			return http.StatusBadRequest, err
		}
		return http.StatusInternalServerError, err
	}

	fmt.Fprintln(out, fmt.Sprintf("\n%s", successfulDeploy))
	return http.StatusOK, err
}

func getDeploymentInfo(reader io.Reader) (S.DeploymentInfo, error) {
	deploymentInfo := S.DeploymentInfo{}
	err := json.NewDecoder(reader).Decode(&deploymentInfo)
	if err != nil {
		return deploymentInfo, err
	}

	getter := geterrors.WrapFunc(func(key string) string {
		if key == "artifact_url" {
			return deploymentInfo.ArtifactURL
		}
		return ""
	})

	getter.Get("artifact_url")

	err = getter.Err("The following properties are missing")
	if err != nil {
		return S.DeploymentInfo{}, err
	}
	return deploymentInfo, nil
}

func isZip(contentType string) bool {
	return contentType == "application/zip"
}

func isJSON(contentType string) bool {
	return contentType == "application/json"
}

func emitDeployFinish(d Deployer, deployEventData S.DeployEventData, out io.Writer, err *error, statusCode *int) {
	d.Log.Debug("emitting a deploy.finish event")
	finishErr := d.EventManager.Emit(S.Event{Type: "deploy.finish", Data: deployEventData})
	if finishErr != nil {
		fmt.Fprintln(out, finishErr)
		*err = errors.Errorf("%s: an error occurred in the deploy.finish event: %s", *err, finishErr)
		*statusCode = http.StatusInternalServerError
	}

}

func emitDeploySuccess(d Deployer, deployEventData S.DeployEventData, out io.Writer, err *error, statusCode *int) {

	deployEvent := S.Event{Type: "deploy.success", Data: deployEventData}

	if *err != nil {
		deployEvent.Type = "deploy.failure"
	}

	d.Log.Debug(fmt.Sprintf("emitting a %s event", deployEvent.Type))
	eventErr := d.EventManager.Emit(deployEvent)
	if eventErr != nil {
		fmt.Fprintln(out, eventErr)
	}
}
