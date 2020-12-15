// Copyright 2020 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charmhub

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/juju/collections/set"
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/utils/v2"
	"github.com/kr/pretty"

	"github.com/juju/juju/charmhub/path"
	"github.com/juju/juju/charmhub/transport"
)

// Action represents the type of refresh is performed.
type Action string

const (
	// InstallAction defines a install action.
	InstallAction Action = "install"

	// DownloadAction defines a download action.
	DownloadAction Action = "download"

	// RefreshAction defines a refresh action.
	RefreshAction Action = "refresh"
)

// Headers represents a series of headers that we would like to pass to the REST
// API.
type Headers = map[string][]string

// RefreshPlatform defines a platform for selecting a specific charm.
type RefreshPlatform struct {
	Architecture string
	OS           string
	Series       string
}

func (p RefreshPlatform) String() string {
	path := p.Architecture
	if p.Series != "" {
		if p.OS != "" {
			path = fmt.Sprintf("%s/%s", path, p.OS)
		}
		path = fmt.Sprintf("%s/%s", path, p.Series)
	}
	return path
}

// RefreshClient defines a client for refresh requests.
type RefreshClient struct {
	path   path.Path
	client RESTClient
	logger Logger
}

// NewRefreshClient creates a RefreshClient for requesting
func NewRefreshClient(path path.Path, client RESTClient, logger Logger) *RefreshClient {
	return &RefreshClient{
		path:   path,
		client: client,
		logger: logger,
	}
}

// RefreshConfig defines a type for building refresh requests.
type RefreshConfig interface {
	// Build a refresh request for sending to the API.
	Build() (transport.RefreshRequest, Headers, error)

	// Ensure that the request back contains the information we requested.
	Ensure([]transport.RefreshResponse) error

	// String describes the underlying refresh config.
	String() string
}

// Refresh is used to refresh installed charms to a more suitable revision.
func (c *RefreshClient) Refresh(ctx context.Context, config RefreshConfig) ([]transport.RefreshResponse, error) {
	c.logger.Tracef("Refresh(%s)", pretty.Sprint(config))
	req, headers, err := config.Build()
	if err != nil {
		return nil, errors.Trace(err)
	}

	httpHeaders := make(http.Header)
	for k, values := range headers {
		for _, value := range values {
			httpHeaders.Add(MetadataHeader, fmt.Sprintf("%s=%s", k, value))
		}
	}

	var resp transport.RefreshResponses
	restResp, err := c.client.Post(ctx, c.path, httpHeaders, req, &resp)

	if err != nil {
		return nil, errors.Trace(err)
	}

	if resultErr := resp.ErrorList.Combine(); resultErr != nil {
		if restResp.StatusCode == http.StatusNotFound {
			return nil, errors.NewNotFound(resultErr, "")
		}
		return nil, errors.Trace(resultErr)
	}

	c.logger.Tracef("Refresh() unmarshalled: %s", pretty.Sprint(resp.Results))
	return resp.Results, config.Ensure(resp.Results)
}

// refreshOne holds the config for making refresh calls to the CharmHub API.
type refreshOne struct {
	ID       string
	Revision int
	Channel  string
	Platform RefreshPlatform
	// instanceKey is a private unique key that we construct for CharmHub API
	// asynchronous calls.
	instanceKey string
}

func (c refreshOne) String() string {
	return fmt.Sprintf("Refresh one (instanceKey: %s): using ID %s revision %+v, with channel %s and platform %v",
		c.instanceKey, c.ID, c.Revision, c.Channel, c.Platform.String())
}

// RefreshOne creates a request config for requesting only one charm.
func RefreshOne(id string, revision int, channel string, platform RefreshPlatform) (RefreshConfig, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, errors.Trace(err)
	}
	uuid, err := utils.NewUUID()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return refreshOne{
		instanceKey: uuid.String(),
		ID:          id,
		Revision:    revision,
		Channel:     channel,
		Platform:    platform,
	}, nil
}

// Build a refresh request that can be past to the API.
func (c refreshOne) Build() (transport.RefreshRequest, Headers, error) {
	return transport.RefreshRequest{
		Context: []transport.RefreshRequestContext{{
			InstanceKey: c.instanceKey,
			ID:          c.ID,
			Revision:    c.Revision,
			Platform: transport.RefreshRequestPlatform{
				OS:           c.Platform.OS,
				Series:       c.Platform.Series,
				Architecture: c.Platform.Architecture,
			},
			TrackingChannel: c.Channel,
			// TODO (stickupkid): We need to model the refreshed date. It's
			// currently optional, but will be required at some point. This
			// is the installed date of the charm on the system.
		}},
		Actions: []transport.RefreshRequestAction{{
			Action:      string(RefreshAction),
			InstanceKey: c.instanceKey,
			ID:          &c.ID,
		}},
	}, constructMetadataHeaders(c.Platform), nil
}

// Ensure that the request back contains the information we requested.
func (c refreshOne) Ensure(responses []transport.RefreshResponse) error {
	for _, resp := range responses {
		if resp.InstanceKey == c.instanceKey {
			return nil
		}
	}
	return errors.NotValidf("refresh action key")
}

type executeOne struct {
	ID       string
	Name     string
	Revision *int
	Channel  *string
	Platform RefreshPlatform
	// instanceKey is a private unique key that we construct for CharmHub API
	// asynchronous calls.
	action      Action
	instanceKey string
}

// InstallOneFromRevision creates a request config using the revision and not
// the channel for requesting only one charm.
func InstallOneFromRevision(name string, revision int, platform RefreshPlatform) (RefreshConfig, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, errors.Trace(err)
	}
	uuid, err := utils.NewUUID()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return executeOne{
		action:      InstallAction,
		instanceKey: uuid.String(),
		Name:        name,
		Revision:    &revision,
		Platform:    platform,
	}, nil
}

// InstallOneFromChannel creates a request config using the channel and not the
// revision for requesting only one charm.
func InstallOneFromChannel(name string, channel string, platform RefreshPlatform) (RefreshConfig, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, errors.Trace(err)
	}
	uuid, err := utils.NewUUID()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return executeOne{
		action:      InstallAction,
		instanceKey: uuid.String(),
		Name:        name,
		Channel:     &channel,
		Platform:    platform,
	}, nil
}

// DownloadOne creates a request config for requesting only one charm.
func DownloadOne(id string, revision int, channel string, platform RefreshPlatform) (RefreshConfig, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, errors.Trace(err)
	}
	uuid, err := utils.NewUUID()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return executeOne{
		action:      DownloadAction,
		instanceKey: uuid.String(),
		ID:          id,
		Revision:    &revision,
		Channel:     &channel,
		Platform:    platform,
	}, nil
}

// DownloadOneFromRevision creates a request config using the revision and not
// the channel for requesting only one charm.
func DownloadOneFromRevision(id string, revision int, platform RefreshPlatform) (RefreshConfig, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, errors.Trace(err)
	}
	uuid, err := utils.NewUUID()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return executeOne{
		action:      DownloadAction,
		instanceKey: uuid.String(),
		ID:          id,
		Revision:    &revision,
		Platform:    platform,
	}, nil
}

// DownloadOneFromChannel creates a request config using the channel and not the
// revision for requesting only one charm.
func DownloadOneFromChannel(id string, channel string, platform RefreshPlatform) (RefreshConfig, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, errors.Trace(err)
	}
	uuid, err := utils.NewUUID()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return executeOne{
		action:      DownloadAction,
		instanceKey: uuid.String(),
		ID:          id,
		Channel:     &channel,
		Platform:    platform,
	}, nil
}

// Build a refresh request that can be past to the API.
func (c executeOne) Build() (transport.RefreshRequest, Headers, error) {
	var id *string
	if c.ID != "" {
		id = &c.ID
	}
	var name *string
	if c.Name != "" {
		name = &c.Name
	}
	return transport.RefreshRequest{
		// Context is required here, even if it looks optional.
		Context: []transport.RefreshRequestContext{},
		Actions: []transport.RefreshRequestAction{{
			Action:      string(c.action),
			InstanceKey: c.instanceKey,
			ID:          id,
			Name:        name,
			Revision:    c.Revision,
			Channel:     c.Channel,
			Platform: &transport.RefreshRequestPlatform{
				OS:           c.Platform.OS,
				Series:       c.Platform.Series,
				Architecture: c.Platform.Architecture,
			},
		}},
	}, constructMetadataHeaders(c.Platform), nil
}

// Ensure that the request back contains the information we requested.
func (c executeOne) Ensure(responses []transport.RefreshResponse) error {
	for _, resp := range responses {
		if resp.InstanceKey == c.instanceKey {
			return nil
		}
	}
	return errors.NotValidf("%v action key", string(c.action))
}

func (c executeOne) String() string {
	var channel string
	if c.Channel != nil {
		channel = *c.Channel
	}
	var using string
	if c.ID != "" {
		using = fmt.Sprintf("ID %s", c.ID)
	} else {
		using = fmt.Sprintf("Name %s", c.Name)
	}
	return fmt.Sprintf("Execute One (action: %s, instanceKey: %s): using %s with revision: %+v, channel %v and platform %s",
		c.action, c.instanceKey, using, c.Revision, channel, c.Platform)
}

type refreshMany struct {
	Configs []RefreshConfig
}

// RefreshMany will compose many refresh configs.
func RefreshMany(configs ...RefreshConfig) RefreshConfig {
	return refreshMany{
		Configs: configs,
	}
}

// Build a refresh request that can be past to the API.
func (c refreshMany) Build() (transport.RefreshRequest, Headers, error) {
	var (
		result          transport.RefreshRequest
		composedHeaders Headers
	)
	for _, config := range c.Configs {
		req, headers, err := config.Build()
		if err != nil {
			return transport.RefreshRequest{}, nil, errors.Trace(err)
		}
		result.Context = append(result.Context, req.Context...)
		result.Actions = append(result.Actions, req.Actions...)
		composedHeaders = composeMetadataHeaders(composedHeaders, headers)
	}
	return result, composedHeaders, nil
}

// Ensure that the request back contains the information we requested.
func (c refreshMany) Ensure(responses []transport.RefreshResponse) error {
	for _, config := range c.Configs {
		if err := config.Ensure(responses); err != nil {
			return errors.Annotatef(err, "missing response")
		}
	}
	return nil
}

func (c refreshMany) String() string {
	plans := make([]string, len(c.Configs))
	for i, config := range c.Configs {
		plans[i] = config.String()
	}
	return strings.Join(plans, "\n")
}

// constructHeaders adds X-Juju-Metadata headers for the charms' unique series
// and architecture values, for example:
//
// X-Juju-Metadata: series=bionic
// X-Juju-Metadata: arch=amd64
// X-Juju-Metadata: series=focal
func constructMetadataHeaders(platform RefreshPlatform) map[string][]string {
	headers := make(map[string][]string)
	if platform.Architecture != "" {
		headers["arch"] = []string{platform.Architecture}
	}
	if platform.OS != "" {
		headers["os"] = []string{platform.OS}
	}
	if platform.Series != "" {
		headers["series"] = []string{platform.Series}
	}
	return headers
}

func composeMetadataHeaders(a, b Headers) Headers {
	result := make(map[string][]string)
	for k, v := range a {
		result[k] = append(result[k], v...)
	}
	for k, v := range b {
		result[k] = append(result[k], v...)
	}
	for k, v := range result {
		result[k] = set.NewStrings(v...).SortedValues()
	}
	return result
}

// validatePlatform ensures that we do not pass "all" as part of platform.
// This function is to help find programming related failures.
func validatePlatform(rp RefreshPlatform) error {
	var msg []string
	if rp.Architecture == "all" {
		msg = append(msg, fmt.Sprintf("Architecture %q", rp.Architecture))
	}
	if rp.OS == "all" {
		msg = append(msg, fmt.Sprintf("OS %q", rp.OS))
	}
	if rp.Series == "all" {
		msg = append(msg, fmt.Sprintf("Series %q", rp.Series))
	}
	if len(msg) > 0 {
		err := errors.Trace(errors.NotValidf(strings.Join(msg, ", ")))
		// Log the error here, trace on this side gets lost when the error
		// goes thru to the client.
		logger := loggo.GetLogger("juju.charmhub.validateplatform")
		logger.Errorf(fmt.Sprintf("%s", err))
		return err
	}
	return nil
}
