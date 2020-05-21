package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"

	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/builds/buildsfakes"
	"github.com/concourse/concourse/atc/creds/credsfakes"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/db/dbfakes"
	"github.com/concourse/concourse/atc/db/lock/lockfakes"
	"github.com/concourse/concourse/atc/engine/enginefakes"
	"github.com/concourse/concourse/atc/lidar"
	"github.com/tedsuo/ifrit"
)

var (
	resourceCheckingInterval time.Duration
	lidarRunnerInterval      time.Duration
	lastRan                  time.Time
	numOfResources           *int
	checkDuration            time.Duration
	fakeChecksStorage        fakeChecksStore
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
	ScheduledChecks []db.Check
}

type resource struct {
	dbfakes.FakeResource
	lastCheckEndTime time.Time
}

func (r *resource) LastCheckEndTime() time.Time {
	return r.lastCheckEndTime
}

func main() {
	// Parse flags that
	err := parseFlags()
	if err != nil {
		fmt.Printf("parse flags: %v\n", err)
		return
	}

	// Set up faking out all of lidar's depdencies, including the database and
	// lock factory. Most of the logic will stay the same, the big difference is
	// that rather than accessing information from the database, it will access
	// the information from global variables
	lidarRunner := setupLidar()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)

	err = lidarRunner.Run(sigs, make(chan struct{}))
	if err != nil {
		fmt.Printf("run lidar: %v\n", err)
	}
}

func parseFlags() error {
	var err error
	resourceCheckingInterval, err = time.ParseDuration(*flag.String("resource-checking-interval", "1m", "the interval resources will be checked on"))
	if err != nil {
		return fmt.Errorf("failed to parse resource-checking-interval %v", err)
	}

	lidarRunnerInterval, err = time.ParseDuration(*flag.String("lidar-runner-interval", "10s", "the interval that the lidar component will run on"))
	if err != nil {
		return fmt.Errorf("failed to parse lidar-runner-interval %v", err)
	}

	numOfResources = flag.Int("number-of-resources", 100, "the number of resources that will be checked")

	checkDuration, err = time.ParseDuration(*flag.String("check-duration", "1s", "how long a check will take to finish"))
	if err != nil {
		return fmt.Errorf("failed to parse check-duration %v", err)
	}

	flag.Parse()

	return nil
}

func setupLidar() ifrit.Runner {
	logger := lagertest.NewTestLogger("lidar-test")
	clock := clock.NewClock()
	notifier := make(chan bool, 1)
	lastRan = time.Now()

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
	var resources []db.Resource
	for i := 1; i < *numOfResources; i++ {
		fakeResource := new(resource)
		fakeResource.TeamNameReturns("team")
		fakeResource.PipelineNameReturns("pipeline")
		fakeResource.NameReturns("resource-" + strconv.Itoa(i))
		fakeResource.TypeReturns("type")
		fakeResource.ResourceConfigScopeIDReturns(i)
		fakeResource.HasWebhookReturns(false)
		fakeResource.CheckEveryReturns("")
		fakeResource.CurrentPinnedVersionReturns(nil)

		resources = append(resources, fakeResource)
	}

	fakeCheckFactory := new(dbfakes.FakeCheckFactory)
	fakeCheckFactory.ResourcesReturns(resources, nil)

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

		// Append our check to the scheduled checks in the mock database
		fakeChecksStorage.ScheduledChecks = append(fakeChecksStorage.ScheduledChecks, fakeCheck)

		fakeChecksStorage.Unlock()

		return nil, true, nil
	}

	fakeCheckFactory.StartedChecksStub = func() ([]db.Check, error) {
		// Lock the mocked database to ensure nothing updates the scheduled checks
		// while we are reading from it
		fakeChecksStorage.Lock()

		// Pull off the scheduled checks from the global variable checks storage
		// and reset the scheduled checks back to empty
		checksToStart := fakeChecksStorage.ScheduledChecks
		fakeChecksStorage.ScheduledChecks = []db.Check{}

		fakeChecksStorage.Unlock()

		return checksToStart, nil
	}

	fakeComponent := new(dbfakes.FakeComponent)
	fakeComponent.PausedReturns(false)

	// The component will determine if it needs to run if the interval elapsed
	// returns back true. This logic is the same as how a real component in
	// Concourse determines it's interval, but is stubbed out to use a global
	// variable to store the time it last ran rather than a database.
	fakeComponent.IntervalElapsedStub = func() bool {
		if time.Now().Sub(lastRan) > lidarRunnerInterval {
			return true
		} else {
			return false
		}
	}

	// Update the global variable for storing when lidar last ran
	fakeComponent.UpdateLastRanStub = func() error {
		lastRan = time.Now()
		return nil
	}

	fakeComponentFactory := new(dbfakes.FakeComponentFactory)
	fakeComponentFactory.FindReturns(fakeComponent, true, nil)

	fakeRunnable := new(enginefakes.FakeRunnable)
	fakeRunnable.RunStub = func(logger lager.Logger) {
		time.Sleep(checkDuration)
	}

	fakeEngine := new(enginefakes.FakeEngine)
	fakeEngine.NewCheckReturns(fakeRunnable)

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
			fakeEngine,
		),
		lidarRunnerInterval,
		fakeNotifications,
		fakeLockFactory,
		fakeComponentFactory,
	)

	return lidarRunner
}
