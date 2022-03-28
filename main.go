package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/elbv2"
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
	elbv2Session   *elbv2.ELBV2
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
		elbv2Session:  elbv2.New(awsSession),
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

func (sh *serviceHandler) getActiveDeploymentId() string {
	fmt.Println(sh.currentOutput.Deployments)
	for _, deployment := range sh.currentOutput.Deployments {
		if aws.StringValue(deployment.Status) == "PRIMARY" {
			return aws.StringValue(deployment.Id)
		}
	}
	return ""
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

	deploymentToCheck := sh.getActiveDeploymentId()
	fmt.Printf("Current Primary deployment is: %s.\n", deploymentToCheck)
	return sh.waitForDeployment(deploymentToCheck)
}

func (sh *serviceHandler) waitForDeployment(deploymentId string) error {
	isComplete := func() (string, bool) {
		for _, deployment := range sh.currentOutput.Deployments {
			if aws.StringValue(deployment.Id) == deploymentId {
				if sh.deploymentState(deployment, "COMPLETED") {
					fmt.Printf("Deployment %s is in state %s.\n", aws.StringValue(deployment.Id), aws.StringValue(deployment.RolloutState))
					return aws.StringValue(deployment.RolloutState), true
				} else {
					return aws.StringValue(deployment.RolloutState), false
				}
			}
		}
		return "NOT_FOUND", false
	}

	// Check the deployment is already finished. No need to wait the first check interval
	if _, ok := isComplete(); ok {
		return nil
	}

	checkTimer := time.NewTicker(time.Second * 10)
	timeout := time.NewTicker(time.Minute * 10)
	defer checkTimer.Stop()
	defer timeout.Stop()

	for {
		select {
		case <-checkTimer.C:
			fmt.Printf("Checking if %s is now COMPLETED.\n", deploymentId)
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
			fmt.Printf("Waiting another %d seconds for deployment %s to change to COMPLETED, currently %s.\n", sh.checkInterval, deploymentId, status)
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

	if len(sh.currentOutput.Events) == 0 {
		fmt.Println("No events found")
		return nil
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
	tasksList, err := sh.session.ListTasks(&ecs.ListTasksInput{
		Cluster:       sh.clusterName,
		ServiceName:   sh.serviceName,
		DesiredStatus: aws.String("STOPPED"),
	})

	if err != nil {
		return err
	}

	if len(tasksList.TaskArns) == 0 {
		fmt.Printf("AWS API returned no STOPPED tasks to show.")
		return nil
	}

	if len(tasksList.TaskArns) < n {
		n = len(tasksList.TaskArns) - 1
		if n == 0 {
			n = 1
		}
	}

	out, err := sh.session.DescribeTasks(&ecs.DescribeTasksInput{
		Tasks:   tasksList.TaskArns[0:n],
		Cluster: sh.clusterName,
	})
	if err != nil {
		return err
	}

	fmt.Println(out)

	return nil
}

func (sh *serviceHandler) checkTargetGroup() (bool, error) {
	if err := sh.refresh(); err != nil {
		return false, err
	}

	if len(sh.currentOutput.LoadBalancers) == 0 {
		fmt.Println("No load balancer to check.")
		return true, nil
	}

	healthOutput, err := sh.elbv2Session.DescribeTargetHealth(
		&elbv2.DescribeTargetHealthInput{
			TargetGroupArn: sh.currentOutput.LoadBalancers[0].TargetGroupArn,
		},
	)
	if err != nil {
		return false, err
	}
	allHealthy := true
	for _, target := range healthOutput.TargetHealthDescriptions {
		if aws.StringValue(target.TargetHealth.State) != "healthy" {
			allHealthy = false
		}
	}

	if !allHealthy {
		return false, nil
	}

	return true, nil
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
	fmt.Println("Looking at deployments status.")
	err = ecsService.checkDeployments()
	if err != nil {
		fmt.Printf("there was an error while checking the state of deployments. Error: %s\n", err)
		exitOut(ecsService, 1)
	}
	fmt.Println("Deployments checked.")

	serviceOk := false
	for !serviceOk {
		// Is the desired count the same as the running count.
		fmt.Println("Checking that running matches desired tasks.")
		err = ecsService.checkPendingCount()
		if err != nil {
			fmt.Printf("There was an error checking the pending count. Error: %s\n", err)
			exitOut(ecsService, 1)
		}
		fmt.Println("Checking the target group is in a good state.")
		ok, err := ecsService.checkTargetGroup()
		if err != nil {
			fmt.Printf("There was an error checking the service target group. Error: %s\n", err)
			exitOut(ecsService, 1)
		}
		if ok {
			serviceOk = true
		} else {
			fmt.Printf("Waiting %d seconds before checking tasks again.\n", *flagCheckInterval)
			time.Sleep(time.Second * time.Duration(*flagCheckInterval))
		}
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
