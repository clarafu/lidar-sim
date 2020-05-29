package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"go.opentelemetry.io/otel/exporter/trace/jaeger"

	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/builds/buildsfakes"
	"github.com/concourse/concourse/atc/creds/credsfakes"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/db/dbfakes"
	"github.com/concourse/concourse/atc/db/lock/lockfakes"
	"github.com/concourse/concourse/atc/engine"
	"github.com/concourse/concourse/atc/engine/enginefakes"
	"github.com/concourse/concourse/atc/lidar"
	"github.com/concourse/concourse/tracing"
	"github.com/tedsuo/ifrit"
)

var (
	resourceCheckingInterval, checkDuration                             time.Duration
	lidarScannerInterval, lidarCheckerInterval, componentRunnerInterval time.Duration
	componentsList                                                      map[string]*component

	maxInFlightChecks    uint64
	numOfResources       int
	fakeChecksStorage    fakeChecksStore
	jaegerURL            string
	randomCheckDurations bool
	allResources         lockedResources
)

// This is used for storing all the checks that are scheduled by the scanner.
// It will be written to when a check is created by the scanner and read off by
// the checker. The checker will reset the scheduled checks when it reads from
// it.
//
// A mutex lock is needed to ensure that each resource scan or check that is
// run in its own goroutine will be able to safely access the scheduled checks
type fakeChecksStore struct {
	sync.Mutex
	ScheduledChecks map[int]db.Check
}

type resource struct {
	dbfakes.FakeResource
	lastCheckEndTime time.Time
}

func (r *resource) LastCheckEndTime() time.Time {
	return r.lastCheckEndTime
}

type component struct {
	dbfakes.FakeComponent
	lastRan  time.Time
	interval time.Duration
}

func (c *component) IntervalElapsed() bool {
	return time.Now().After(c.lastRan.Add(c.interval))
}

// Update the last ran value on the component
func (c *component) UpdateLastRan() error {
	c.lastRan = time.Now()
	return nil
}

// Is constructed of a map of resource IDs to the resource struct, in order to
// easily access each resource by it's ID. It contains a mutex lock to ensure
// that the map is only getting accessed once at a time.
type lockedResources struct {
	sync.Mutex
	resources map[int]*resource
}

type fakeEngine struct {
	enginefakes.FakeEngine
}

func (e *fakeEngine) NewCheck(check db.Check) engine.Runnable {
	return &fakeEngineCheck{
		FakeRunnable: new(enginefakes.FakeRunnable),
		resourceID:   check.ID(),
	}
}

type fakeEngineCheck struct {
	*enginefakes.FakeRunnable
	resourceID int
}

func (e *fakeEngineCheck) Run(logger lager.Logger) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	resourceCheckDuration := checkDuration
	if randomCheckDurations {
		randomDuration := r.Int63n(checkDuration.Nanoseconds())
		resourceCheckDuration = time.Duration(randomDuration)
	}

	time.Sleep(resourceCheckDuration)

	allResources.resources[e.resourceID].lastCheckEndTime = time.Now()
	fakeChecksStorage.Lock()
	delete(fakeChecksStorage.ScheduledChecks, e.resourceID)
	fakeChecksStorage.Unlock()
}

func main() {
	// Parse flags
	parseFlags()

	// Set up tracing if configured with jaeger url
	if jaegerURL != "" {
		err := setupTracing()
		if err != nil {
			fmt.Printf("setup tracing: %v\n", err)
			return
		}
	}

	// Set up faking out all of lidar's depedencies, including the database and
	// lock factory. Most of the logic will stay the same, the big difference is
	// that rather than accessing information from the database, it will access
	// the information from global variables
	lidarRunner := setupLidar()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)

	err := lidarRunner.Run(sigs, make(chan struct{}))
	if err != nil {
		fmt.Printf("run lidar: %v\n", err)
	}
}

func parseFlags() {
	flag.IntVar(&numOfResources, "numberOfResources", 100, "the number of resources that will be checked")
	flag.StringVar(&jaegerURL, "jaegerURL", "", "jaeger URL that the traces will be sent to")
	flag.DurationVar(&resourceCheckingInterval, "resourceCheckingInterval", 1*time.Minute, "the interval resources will be checked on")
	flag.DurationVar(&lidarScannerInterval, "lidarScannerInterval", 1*time.Minute, "the interval which the llidar scanner will run on")
	flag.DurationVar(&lidarCheckerInterval, "lidarCheckerInterval", 10*time.Second, "the interval which the lidar checker will run on")
	flag.DurationVar(&componentRunnerInterval, "componentRunnerInterval", 10*time.Second, "the interval which the component runner will run on")
	flag.DurationVar(&checkDuration, "checkDuration", 1*time.Second, "how long a check will take to finish")
	flag.BoolVar(&randomCheckDurations, "randomCheckDurations", false, "if true, will cause the durations of checks to be randomized with a maximum duration of the check duration flag (default is 1 second)")
	flag.Uint64Var(&maxInFlightChecks, "maxInFlightChecks", 32, "maximum number of checks that can run at the same time")

	flag.Parse()
}

func setupTracing() error {
	spanSyncer, err := (tracing.Jaeger{
		Endpoint: jaegerURL + "/api/traces",
		Service:  "lidar-simulation",
	}).Exporter()
	if err != nil {
		return fmt.Errorf("jaeger exporter: %v", err)
	}

	exporter := spanSyncer.(*jaeger.Exporter)

	err = tracing.ConfigureTracer(exporter)
	if err != nil {
		return fmt.Errorf("configure tracer: %v", err)
	}

	return nil
}

func setupLidar() ifrit.Runner {
	logger := lagertest.NewTestLogger("lidar-test")
	clock := clock.NewClock()
	notifier := make(chan bool, 1)

	scheduledChecksMap := make(map[int]db.Check)
	fakeChecksStorage.ScheduledChecks = scheduledChecksMap

	fakeSecrets := new(credsfakes.FakeSecrets)
	fakeLockFactory := new(lockfakes.FakeLockFactory)
	fakeLock := new(lockfakes.FakeLock)
	fakeLock.ReleaseReturns(nil)
	fakeLockFactory.AcquireReturns(fakeLock, true, nil)

	fakeNotifications := new(buildsfakes.FakeNotifications)
	fakeNotifications.ListenReturns(notifier, nil)
	fakeNotifications.UnlistenReturns(nil)

	// Create a slice of the number of resources that are wanted to run this test
	// with. Default is 100 resources
	resources := make(map[int]*resource)
	for i := 1; i < numOfResources; i++ {
		fakeResource := new(resource)
		fakeResource.TeamNameReturns("team")
		fakeResource.PipelineNameReturns("pipeline")
		fakeResource.NameReturns("resource-" + strconv.Itoa(i))
		fakeResource.TypeReturns("type")
		fakeResource.ResourceConfigScopeIDReturns(i)
		fakeResource.HasWebhookReturns(false)
		fakeResource.CheckEveryReturns("")
		fakeResource.CurrentPinnedVersionReturns(nil)

		resources[i] = fakeResource
	}

	allResources.resources = resources

	fakeCheckFactory := new(dbfakes.FakeCheckFactory)
	fakeCheckFactory.ResourcesStub = func() ([]db.Resource, error) {
		allResources.Lock()

		var resources []db.Resource
		for _, resource := range allResources.resources {
			resources = append(resources, resource)
		}

		allResources.Unlock()

		return resources, nil
	}

	// Do not check custom resource types. Can be added later if we want to see
	// how the speed of checks are affected by custom resource types
	fakeCheckFactory.ResourceTypesReturns(nil, nil)

	fakeCheckFactory.TryCreateCheckStub = func(ctx context.Context, checkable db.Checkable, rts db.ResourceTypes, fromVersion atc.Version, manuallyTriggered bool) (db.Check, bool, error) {
		// Lock the mocked out checks database to ensure thread safety between
		// multiple checks attempting to write/read from the scheduled checks slice
		// at once
		fakeChecksStorage.Lock()

		fakeCheck := new(dbfakes.FakeCheck)
		fakeCheck.TeamNameReturns("team")
		fakeCheck.PipelineNameReturns("pipeline")
		fakeCheck.IDReturns(checkable.ResourceConfigScopeID())
		fakeCheck.ResourceConfigScopeIDReturns(checkable.ResourceConfigScopeID())
		fakeCheck.SpanContextReturns(db.NewSpanContext(ctx))

		// Append our check to the scheduled checks in the mock database
		if _, found := fakeChecksStorage.ScheduledChecks[checkable.ResourceConfigScopeID()]; !found {
			fakeChecksStorage.ScheduledChecks[checkable.ResourceConfigScopeID()] = fakeCheck
		}

		fakeChecksStorage.Unlock()

		return nil, true, nil
	}

	fakeCheckFactory.StartedChecksStub = func() ([]db.Check, error) {
		// Lock the mocked database to ensure nothing updates the scheduled checks
		// while we are reading from it
		fakeChecksStorage.Lock()

		// Pull off the scheduled checks from the global variable checks storage
		// and reset the scheduled checks back to empty

		var checksToStart []db.Check
		for _, j := range fakeChecksStorage.ScheduledChecks {
			checksToStart = append(checksToStart, j)
		}

		fakeChecksStorage.Unlock()

		return checksToStart, nil
	}

	fakeScannerComponent := new(component)
	fakeScannerComponent.PausedReturns(false)
	fakeScannerComponent.interval = lidarScannerInterval

	fakeCheckerComponent := new(component)
	fakeCheckerComponent.PausedReturns(false)
	fakeCheckerComponent.interval = lidarCheckerInterval

	// Construct the list of components with the lidar scanner and lidar checker fakes
	componentsList = make(map[string]*component)
	componentsList[atc.ComponentLidarScanner] = fakeScannerComponent
	componentsList[atc.ComponentLidarChecker] = fakeCheckerComponent

	fakeComponentFactory := new(dbfakes.FakeComponentFactory)
	fakeComponentFactory.FindStub = func(name string) (db.Component, bool, error) {
		component, found := componentsList[name]
		if !found {
			return nil, false, errors.New(fmt.Sprintf("component %s does not exist", name))
		}

		return component, true, nil
	}

	lidarRunner := lidar.NewRunner(
		logger,
		clock,
		lidar.NewScanner(
			logger.Session("scanner"),
			fakeCheckFactory,
			fakeSecrets,
			1*time.Hour,
			resourceCheckingInterval,
			1*time.Minute,
		),
		lidar.NewChecker(
			logger.Session("checker"),
			fakeCheckFactory,
			new(fakeEngine),
			maxInFlightChecks,
		),
		componentRunnerInterval,
		fakeNotifications,
		fakeLockFactory,
		fakeComponentFactory,
	)

	return lidarRunner
}
