// Copyright 2021 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package agent

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/juju/charm/v9"
	"github.com/juju/collections/set"
	"github.com/juju/errors"
	"github.com/juju/schema"
	"gopkg.in/juju/environschema.v1"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/api"
	"github.com/juju/juju/apiserver"
	"github.com/juju/juju/apiserver/facades/client/application"
	"github.com/juju/juju/apiserver/facades/client/charms/interfaces"
	"github.com/juju/juju/apiserver/facades/client/charms/services"
	"github.com/juju/juju/charmhub"
	coreapplication "github.com/juju/juju/core/application"
	corearch "github.com/juju/juju/core/arch"
	corecharm "github.com/juju/juju/core/charm"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/core/network"
	coreos "github.com/juju/juju/core/os"
	coreseries "github.com/juju/juju/core/series"
	"github.com/juju/juju/environs/bootstrap"
	"github.com/juju/juju/state"
	statestorage "github.com/juju/juju/state/storage"
)

const controllerCharmURL = "ch:juju-controller"

func (c *BootstrapCommand) deployControllerCharm(st *state.State, cons constraints.Value, charmRisk string, isCAAS bool, unitPassword string) (resultErr error) {
	arch := corearch.DefaultArchitecture
	series := coreseries.LatestLts()
	if cons.HasArch() {
		arch = *cons.Arch
	}

	var controllerUnit *state.Unit
	controllerAddress := ""
	if isCAAS {
		s, err := st.CloudService(st.ControllerUUID())
		if err != nil {
			return errors.Trace(err)
		}
		hp := network.SpaceAddressesWithPort(s.Addresses(), 0)
		addr := hp.AllMatchingScope(network.ScopeMatchCloudLocal)
		if len(addr) > 0 {
			controllerAddress = addr[0]
		}
		logger.Debugf("CAAS controller address %v", controllerAddress)
		// For k8s, we need to set the charm unit agent password.
		defer func() {
			if resultErr == nil && controllerUnit != nil {
				resultErr = controllerUnit.SetPassword(unitPassword)
			}
		}()
	} else {
		m, err := st.Machine(agent.BootstrapControllerId)
		if err != nil {
			return errors.Trace(err)
		}
		defer func() {
			if resultErr == nil && controllerUnit != nil {
				resultErr = controllerUnit.AssignToMachine(m)
			}
		}()
		series = m.Series()
		pa, err := m.PublicAddress()
		if err != nil && !network.IsNoAddressError(err) {
			return errors.Trace(err)
		}
		if err == nil {
			controllerAddress = pa.Value
		}
	}

	// First try using a local charm specified at bootstrap time.
	source := "local"
	curl, origin, err := populateLocalControllerCharm(st, c.DataDir(), series, arch)
	if err != nil && !errors.IsNotFound(err) {
		return errors.Annotate(err, "deploying local controller charm")
	}
	// If no local charm, use the one from charmhub.
	if err != nil {
		source = "store"
		if curl, origin, err = populateStoreControllerCharm(st, charmRisk, series, arch); err != nil {
			return errors.Annotate(err, "deploying charmhub controller charm")
		}
	}

	// Once the charm is added, set up the controller application.
	if controllerUnit, err = addControllerApplication(st, curl, *origin, controllerAddress, series); err != nil {
		return errors.Annotate(err, "cannot add controller application")
	}
	logger.Debugf("Successfully deployed %s Juju controller charm", source)
	return nil
}

// These are patched for testing.
var (
	newCharmRepo = func(cfg services.CharmRepoFactoryConfig) (corecharm.Repository, error) {
		charmRepoFactory := services.NewCharmRepoFactory(cfg)
		return charmRepoFactory.GetCharmRepository(corecharm.CharmHub)
	}
	newCharmDownloader = func(cfg services.CharmDownloaderConfig) (interfaces.Downloader, error) {
		return services.NewCharmDownloader(cfg)
	}
)

// populateStoreControllerCharm downloads and stores the controller charm from charmhub.
func populateStoreControllerCharm(st *state.State, charmRisk, series, arch string) (*charm.URL, *corecharm.Origin, error) {
	model, err := st.Model()
	if err != nil {
		return nil, nil, err
	}

	stateBackend := &stateShim{st}
	charmRepo, err := newCharmRepo(services.CharmRepoFactoryConfig{
		Logger:       logger,
		Transport:    charmhub.DefaultHTTPTransport(logger),
		StateBackend: stateBackend,
		ModelBackend: model,
	})
	if err != nil {
		return nil, nil, err
	}
	curl := charm.MustParseURL(controllerCharmURL)
	channel := corecharm.MakeRiskOnlyChannel(charmRisk)
	origin := corecharm.Origin{
		Source:  corecharm.CharmHub,
		Type:    "charm",
		Channel: &channel,
		Platform: corecharm.Platform{
			Architecture: arch,
			OS:           strings.ToLower(coreos.Ubuntu.String()),
			Series:       charmhub.NotAvailable,
		},
	}

	var supportedSeries []string
	curl, origin, supportedSeries, err = charmRepo.ResolveWithPreferredChannel(curl, origin, nil)
	if err != nil {
		return nil, nil, errors.Annotatef(err, "resolving %q", controllerCharmURL)
	}
	// We prefer the latest LTS series but if the controller charm supported series
	// matches that of the machine, use that one. The controller charm doesn't have
	// any series specific code.
	if charmSeries := set.NewStrings(supportedSeries...); charmSeries.Contains(series) {
		curl = curl.WithSeries(series)
	} else if charmSeries.Contains(coreseries.LatestLts()) {
		curl = curl.WithSeries(coreseries.LatestLts())
	} else {
		// Fallback in case controller charm is out of date.
		curl = curl.WithSeries("focal")
	}

	storageFactory := func(modelUUID string) services.Storage {
		return statestorage.NewStorage(model.UUID(), st.MongoSession())
	}
	charmDownloader, err := newCharmDownloader(services.CharmDownloaderConfig{
		Logger:         logger,
		Transport:      charmhub.DefaultHTTPTransport(logger),
		StorageFactory: storageFactory,
		StateBackend:   stateBackend,
		ModelBackend:   model,
	})
	if err != nil {
		return nil, nil, err
	}
	resOrigin, err := charmDownloader.DownloadAndStore(curl, origin, nil, false)
	if err != nil {
		return nil, nil, err
	}

	return curl, &resOrigin, nil
}

// stateShim allows us to use a real state instance with the charm services logic.
type stateShim struct {
	*state.State
}

func (st *stateShim) PrepareCharmUpload(curl *charm.URL) (services.UploadedCharm, error) {
	return st.State.PrepareCharmUpload(curl)
}

func (st *stateShim) UpdateUploadedCharm(info state.CharmInfo) (services.UploadedCharm, error) {
	return st.State.UpdateUploadedCharm(info)
}

// populateLocalControllerCharm downloads and stores a local controller charm archive.
func populateLocalControllerCharm(st *state.State, dataDir, series, arch string) (*charm.URL, *corecharm.Origin, error) {
	controllerCharmPath := filepath.Join(dataDir, "charms", bootstrap.ControllerCharmArchive)
	_, err := os.Stat(controllerCharmPath)
	if os.IsNotExist(err) {
		return nil, nil, errors.NotFoundf(controllerCharmPath)
	}
	if err != nil {
		return nil, nil, errors.Trace(err)
	}

	curl, err := addLocalControllerCharm(st, series, controllerCharmPath)
	if err != nil {
		return nil, nil, errors.Annotatef(err, "cannot store controller charm at %q", controllerCharmPath)
	}
	logger.Debugf("Successfully deployed local Juju controller charm")
	origin := corecharm.Origin{
		Source: corecharm.Local,
		Type:   "charm",
		Platform: corecharm.Platform{
			Architecture: arch,
			OS:           "ubuntu",
			Series:       series,
		},
	}
	return curl, &origin, nil
}

// addLocalControllerCharm adds the specified local charm to the controller.
func addLocalControllerCharm(st *state.State, series, charmFileName string) (*charm.URL, error) {
	archive, err := charm.ReadCharmArchive(charmFileName)
	if err != nil {
		return nil, errors.Errorf("invalid charm archive: %v", err)
	}

	name := archive.Meta().Name
	if name != bootstrap.ControllerCharmName {
		return nil, errors.Errorf("unexpected controller charm name %q", name)
	}

	// Reserve a charm URL for it in state.
	curl := &charm.URL{
		Schema:   charm.Local.String(),
		Name:     name,
		Revision: archive.Revision(),
		Series:   series,
	}
	curl, err = st.PrepareLocalCharmUpload(curl)
	if err != nil {
		return nil, errors.Trace(err)
	}

	// Now we need to repackage it with the reserved URL, upload it to
	// provider storage and update the state.
	err = apiserver.RepackageAndUploadCharm(st, archive, curl)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return curl, nil
}

// addControllerApplication deploys and configures the controller application.
func addControllerApplication(st *state.State, curl *charm.URL, origin corecharm.Origin, address, series string) (*state.Unit, error) {
	ch, err := st.Charm(curl)
	if err != nil {
		return nil, errors.Trace(err)
	}
	cfg := charm.Settings{
		"is-juju": true,
	}
	controllerCfg, err := st.ControllerConfig()
	if err != nil {
		return nil, errors.Trace(err)
	}
	cfg["identity-provider-url"] = controllerCfg.IdentityURL()
	addr := controllerCfg.PublicDNSAddress()
	if addr == "" {
		addr = address
	}
	if addr != "" {
		cfg["controller-url"] = api.ControllerAPIURL(addr, controllerCfg.APIPort())
	}

	configSchema := environschema.Fields{
		application.TrustConfigOptionName: {
			Type: environschema.Tbool,
		},
	}
	appCfg, err := coreapplication.NewConfig(nil, configSchema, schema.Defaults{
		application.TrustConfigOptionName: true,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}

	app, err := st.AddApplication(state.AddApplicationArgs{
		Name:              bootstrap.ControllerApplicationName,
		Series:            series,
		Charm:             ch,
		CharmOrigin:       application.StateCharmOrigin(origin),
		CharmConfig:       cfg,
		ApplicationConfig: appCfg,
		NumUnits:          1,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return st.Unit(app.Name() + "/0")
}
