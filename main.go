package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecs"
)

var (
	version = "development"

	flagServiceName   = flag.String("service", "", "Service Name to track")
	flagClusterName   = flag.String("cluster", "", "Cluster to find service")
	flagCheckInterval = flag.Int("check", 10, "Seconds between checks. Consider the ECS API rate limits heavily")
	flagTimeout       = flag.Int("timeout", 10, "Timeout in minutes. If the deployment is still happening after the timeout, it will be considered a failure.")
	flagVerbose       = flag.Bool("V", false, "Verbose logging")
	flagVersion       = flag.Bool("v", false, "Show version")
	flagHelp          = flag.Bool("h", false, "Help menu")
)

type serviceHandler struct {
	session        *ecs.ECS
	serviceName    *string
	clusterName    *string
	checkInterval  int
	checkTimeout   int
	versboseOutput bool

	describeServiceInput *ecs.DescribeServicesInput
	currentOutput        *ecs.Service
}

func newServiceHandler(awsSession *session.Session, serviceName, clusterName string, checkInternval, checktimeout int) *serviceHandler {
	return &serviceHandler{
		session:       ecs.New(awsSession),
		serviceName:   aws.String(serviceName),
		clusterName:   aws.String(clusterName),
		checkInterval: checkInternval,
		checkTimeout:  checktimeout,
		describeServiceInput: &ecs.DescribeServicesInput{
			Cluster:  aws.String(clusterName),
			Services: []*string{aws.String(serviceName)},
		},
		versboseOutput: false,
	}
}

func (sh *serviceHandler) enableVerbosePrinting(trigger bool) {
	sh.versboseOutput = trigger
}

func (sh *serviceHandler) deploymentState(deployment *ecs.Deployment, desiredState string) bool {
	return aws.StringValue(deployment.RolloutState) == desiredState
}

func (sh *serviceHandler) describeServiceRaw() (*ecs.DescribeServicesOutput, error) {
	return sh.session.DescribeServices(sh.describeServiceInput)
}

func (sh *serviceHandler) refresh() error {
	output, err := sh.session.DescribeServices(sh.describeServiceInput)
	if err != nil {
		return err
	}
	if len(output.Services) == 0 {
		return fmt.Errorf("service not found")
	}
	sh.currentOutput = output.Services[0]
	return nil
}

func (sh *serviceHandler) printDetails() {
	events := []*ecs.ServiceEvent{}
	details := sh.currentOutput
	details.Events = events
	fmt.Println(details)
}

func (sh *serviceHandler) checkDeployments() error {
	if err := sh.refresh(); err != nil {
		return err
	}

	for _, deployment := range sh.currentOutput.Deployments {
		if sh.deploymentState(deployment, "COMPLETED") {
			continue
		}
		if sh.deploymentState(deployment, "IN_PROGRESS") {
			fmt.Println("Found in progress deployment", *deployment.Id)
			err := sh.waitForDeployment(aws.StringValue(deployment.Id))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (sh *serviceHandler) waitForDeployment(deploymentId string) error {
	// Check the deployment is already finished. No need to wait the first check interval
	isComplete := func() (string, bool) {
		for _, deployment := range sh.currentOutput.Deployments {
			if aws.StringValue(deployment.Id) == deploymentId {
				if sh.deploymentState(deployment, "COMPLETED") {
					fmt.Println("Deployment is complete")
					return aws.StringValue(deployment.RolloutState), true
				} else {
					return aws.StringValue(deployment.RolloutState), false
				}
			}
		}
		return "NOT_FOUND", false
	}

	checkTimer := time.NewTicker(time.Second * 10)
	timeout := time.NewTicker(time.Minute * 10)
	defer checkTimer.Stop()
	defer timeout.Stop()

	for {
		select {
		case <-checkTimer.C:
			fmt.Printf("Checking if %s is now COMPLETE\n", deploymentId)
			if err := sh.refresh(); err != nil {
				return err
			}
			status, ok := isComplete()
			if ok {
				return nil
			}
			if status == "NOT_FOUND" {
				return fmt.Errorf("deployment disappeared")
			}
			fmt.Printf("Waiting another %d seconds for deployment %s to change to COMPLETED, currently %s\n", sh.checkInterval, deploymentId, status)
		case <-timeout.C:
			return fmt.Errorf("timeouted out waiting for deployment to happen")
		}
	}
}

func (sh *serviceHandler) checkPendingCount() error {
	if err := sh.refresh(); err != nil {
		return err
	}

	if aws.Int64Value(sh.currentOutput.DesiredCount) != aws.Int64Value(sh.currentOutput.RunningCount) {
		err := sh.waitForRunningToMatchDesired()
		if err != nil {
			return err
		}
	}

	return nil
}

func (sh *serviceHandler) waitForRunningToMatchDesired() error {
	isComplete := func() bool {
		return aws.Int64Value(sh.currentOutput.DesiredCount) == aws.Int64Value(sh.currentOutput.RunningCount)
	}

	if isComplete() {
		return nil
	}

	checkTimer := time.NewTicker(time.Second * 10)
	timeout := time.NewTicker(time.Minute * 10)
	defer checkTimer.Stop()
	defer timeout.Stop()

	for {
		select {
		case <-checkTimer.C:
			fmt.Println("Checking to see if RUNNING count matches DESIRED count.")
			sh.refresh()
			if isComplete() {
				fmt.Println("Running count is currently correct, waiting 15 seconds to see it stays online.")
				time.Sleep(time.Second * 15)
				if isComplete() {
					return nil
				}
			}
			fmt.Printf("Waiting another %d seconds for running to match desired, currently desired: %d and running: %d.\n", sh.checkInterval, *sh.currentOutput.DesiredCount, *sh.currentOutput.RunningCount)
		case <-timeout.C:
			return fmt.Errorf("timeouted out waiting for desired to match running")
		}
	}
}

func (sh *serviceHandler) printLastNEvents(n int) error {
	if err := sh.refresh(); err != nil {
		return err
	}
	if len(sh.currentOutput.Events) < n {
		n = len(sh.currentOutput.Events) - 1
		if n == 0 {
			n = 1
		}
	}

	fmt.Println(sh.currentOutput.Events[0:n])

	return nil
}

func (sh *serviceHandler) printLastNTasks(n int) error {
	tasksLit, err := sh.session.ListTasks(&ecs.ListTasksInput{
		Cluster:       sh.clusterName,
		ServiceName:   sh.serviceName,
		DesiredStatus: aws.String("STOPPED"),
	})

	if err != nil {
		return err
	}

	if len(tasksLit.TaskArns) < n {
		n = len(tasksLit.TaskArns) - 1
		if n == 0 {
			n = 1
		}
	}
	println(len(tasksLit.TaskArns))
	println(n)

	out, err := sh.session.DescribeTasks(&ecs.DescribeTasksInput{
		Tasks:   tasksLit.TaskArns[0:n],
		Cluster: sh.clusterName,
	})
	if err != nil {
		return err
	}

	fmt.Println(out)

	return nil
}

func main() {
	flag.Parse()
	if *flagHelp {
		flag.PrintDefaults()
		return
	}

	if *flagVersion {
		showVersion()
		return
	}

	awsSession, err := session.NewSession()
	if err != nil {
		fmt.Println("There was an error starting the AWS Session. Error:", err)
		os.Exit(1)
	}
	ecsService := newServiceHandler(awsSession, *flagServiceName, *flagClusterName, *flagCheckInterval, *flagTimeout)
	ecsService.enableVerbosePrinting(*flagVerbose)

	// check that we can lookup the service in AWS ECS
	serviceDetails, err := ecsService.describeServiceRaw()
	if err != nil {
		fmt.Printf("Error describing service. Error: %s", err)
		os.Exit(1)
	}
	if len(serviceDetails.Services) == 0 {
		fmt.Println("Service not found")
		verbosePrint("%s\n", serviceDetails)
		os.Exit(1)
	}

	err = ecsService.refresh()
	if err != nil {
		fmt.Printf("Failed to refresh service details. Error: %s\n", err)
		os.Exit(1)
	}

	if *flagVerbose {
		ecsService.printDetails()
	}

	// Is there a deployment on going?
	fmt.Println("Checking if there is a pending deployment.")
	err = ecsService.checkDeployments()
	if err != nil {
		fmt.Printf("there was an error while checking the state of deployments. Error: %s\n", err)
		exitOut(ecsService, 1)
	}
	fmt.Println("Deployments checked.")
	// Is the desired count the same as the running count.
	fmt.Println("Checking that running matches desired tasks.")
	err = ecsService.checkPendingCount()
	if err != nil {
		fmt.Printf("There was an error checking the pending out. Error: %s\n", err)
		exitOut(ecsService, 1)
	}
	fmt.Println("Service looks good.")
}

func showVersion() {
	fmt.Println(version)
}

func verbosePrint(format string, args ...interface{}) {
	if *flagVerbose {
		fmt.Printf(format, args...)
	}
}

func exitOut(ecsService *serviceHandler, code int) {
	fmt.Printf("Here is some trouble shooting information for %s.\n", *ecsService.serviceName)
	ecsService.printLastNEvents(10)

	fmt.Println("Historical events, showing maximum 10:")
	if err := ecsService.printLastNEvents(10); err != nil {
		fmt.Printf("There was an error listing the events. Error: %s", err)
	}
	fmt.Println("STOPPED services, showing maximum 5:")
	if err := ecsService.printLastNTasks(5); err != nil {
		fmt.Printf("There was an error listing the STOPPED tasks. Error: %s", err)
	}
	os.Exit(code)
}
