package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/golang-jwt/jwt/v4"
	gt "github.com/google/go-github/v59/github"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/apis/pipelinesascode/keys"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/apis/pipelinesascode/v1alpha1"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/provider/github"
)

type Install struct {
	request   *http.Request
	run       *params.Run
	repo      *v1alpha1.Repository
	ghClient  *github.Provider
	namespace string

	repoList []string
}

func NewInstallation(req *http.Request, run *params.Run, repo *v1alpha1.Repository, gh *github.Provider, namespace string) *Install {
	if req == nil {
		req = &http.Request{}
	}
	return &Install{
		request:   req,
		run:       run,
		repo:      repo,
		ghClient:  gh,
		namespace: namespace,
	}
}

func (ip *Install) GetAndUpdateInstallationID(ctx context.Context) (string, string, int64, error) {
	var (
		enterpriseHost, token string
		installationID        int64
	)
	jwtToken, err := ip.GenerateJWT(ctx)
	if err != nil {
		return "", "", 0, err
	}

	installationURL := *ip.ghClient.APIURL + keys.InstallationURL
	enterpriseHost = ip.request.Header.Get("X-GitHub-Enterprise-Host")
	if enterpriseHost != "" {
		// NOTE: Hopefully this works even when the ghe URL is on another host than the api URL
		installationURL = "https://" + enterpriseHost + "/api/v3" + keys.InstallationURL
	}

	res, err := GetReponse(ctx, http.MethodGet, installationURL, jwtToken, ip.run)
	if err != nil {
		return "", "", 0, err
	}

	if res.StatusCode >= 300 {
		return "", "", 0, fmt.Errorf("Non-OK HTTP status while getting installation URL: %s : %d", installationURL, res.StatusCode)
	}

	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return "", "", 0, err
	}

	installationData := []gt.Installation{}
	if err = json.Unmarshal(data, &installationData); err != nil {
		return "", "", 0, err
	}

	/* each installationID can have list of repository
	ref: https://docs.github.com/en/developers/apps/building-github-apps/authenticating-with-github-apps#authenticating-as-an-installation ,
	     https://docs.github.com/en/rest/apps/installations?apiVersion=2022-11-28#list-repositories-accessible-to-the-app-installation */
	for i := range installationData {
		if installationData[i].ID == nil {
			return "", "", 0, fmt.Errorf("installation ID is nil")
		}
		if *installationData[i].ID != 0 {
			token, err = ip.ghClient.GetAppToken(ctx, ip.run.Clients.Kube, enterpriseHost, *installationData[i].ID, ip.namespace)
			if err != nil {
				return "", "", 0, err
			}
		}
		exist, err := ip.listRepos(ctx)
		if err != nil {
			return "", "", 0, err
		}
		if exist {
			installationID = *installationData[i].ID
			break
		}
	}
	return enterpriseHost, token, installationID, nil
}

func (ip *Install) listRepos(ctx context.Context) (bool, error) {
	if ip.repoList == nil {
		var err error
		ip.repoList, err = github.ListRepos(ctx, ip.ghClient)
		if err != nil {
			return false, err
		}
	}
	for i := range ip.repoList {
		// If URL matches with repo spec url then we can break for loop
		if ip.repoList[i] == ip.repo.Spec.URL {
			return true, nil
		}
	}
	return false, nil
}

type JWTClaim struct {
	Issuer int64 `json:"iss"`
	jwt.RegisteredClaims
}

func (ip *Install) GenerateJWT(ctx context.Context) (string, error) {
	// TODO: move this out of here
	gh := github.New()
	gh.Run = ip.run
	applicationID, privateKey, err := gh.GetAppIDAndPrivateKey(ctx, ip.namespace, ip.run.Clients.Kube)
	if err != nil {
		return "", err
	}

	// The expirationTime claim identifies the expiration time on or after which the JWT MUST NOT be accepted for processing.
	// Value cannot be longer duration.
	// See https://datatracker.ietf.org/doc/html/rfc7519#section-4.1.4
	expirationTime := time.Now().Add(5 * time.Minute)
	claims := &JWTClaim{
		Issuer: applicationID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expirationTime),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	parsedPK, err := jwt.ParseRSAPrivateKeyFromPEM(privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %w", err)
	}

	tokenString, err := token.SignedString(parsedPK)
	if err != nil {
		return "", fmt.Errorf("failed to sign private key: %w", err)
	}
	return tokenString, nil
}

func GetReponse(ctx context.Context, method, urlData, jwtToken string, run *params.Run) (*http.Response, error) {
	rawurl, err := url.Parse(urlData)
	if err != nil {
		return nil, err
	}

	newreq, err := http.NewRequestWithContext(ctx, method, rawurl.String(), nil)
	if err != nil {
		return nil, err
	}
	newreq.Header = map[string][]string{
		"Accept":        {"application/vnd.github+json"},
		"Authorization": {fmt.Sprintf("Bearer %s", jwtToken)},
	}
	res, err := run.Clients.HTTP.Do(newreq)
	return res, err
}
