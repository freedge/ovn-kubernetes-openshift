package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"text/template"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	controllerManager "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-controller-manager"
	ovnnode "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kexec "k8s.io/utils/exec"
)

const (
	// CustomAppHelpTemplate helps in grouping options to ovnkube
	CustomAppHelpTemplate = `NAME:
   {{.Name}} - {{.Usage}}

USAGE:
   {{.HelpName}} [global options]

VERSION:
   {{.Version}}{{if .Description}}

DESCRIPTION:
   {{.Description}}{{end}}

COMMANDS:{{range .VisibleCategories}}{{if .Name}}

   {{.Name}}:{{end}}{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}

GLOBAL OPTIONS:{{range $title, $category := getFlagsByCategory}}
   {{upper $title}}
   {{range $index, $option := $category}}{{if $index}}
   {{end}}{{$option}}{{end}}
   {{end}}`
)

func getFlagsByCategory() map[string][]cli.Flag {
	m := map[string][]cli.Flag{}
	m["Generic Options"] = config.CommonFlags
	m["CNI Options"] = config.CNIFlags
	m["K8s-related Options"] = config.K8sFlags
	m["OVN Northbound DB Options"] = config.OvnNBFlags
	m["OVN Southbound DB Options"] = config.OvnSBFlags
	m["OVN Gateway Options"] = config.OVNGatewayFlags
	m["Master HA Options"] = config.MasterHAFlags
	m["OVN Kube Node Options"] = config.OvnKubeNodeFlags
	m["Monitoring Options"] = config.MonitoringFlags
	m["IPFIX Flow Tracing Options"] = config.IPFIXFlags

	return m
}

// borrowed from cli packages' printHelpCustom()
func printOvnKubeHelp(out io.Writer, templ string, data interface{}, customFunc map[string]interface{}) {
	funcMap := template.FuncMap{
		"join":               strings.Join,
		"upper":              strings.ToUpper,
		"getFlagsByCategory": getFlagsByCategory,
	}
	for key, value := range customFunc {
		funcMap[key] = value
	}

	w := tabwriter.NewWriter(out, 1, 8, 2, ' ', 0)
	t := template.Must(template.New("help").Funcs(funcMap).Parse(templ))
	err := t.Execute(w, data)
	if err == nil {
		_ = w.Flush()
	}
}

func main() {
	cli.HelpPrinterCustom = printOvnKubeHelp
	c := cli.NewApp()
	c.Name = "ovnkube"
	c.Usage = "run ovnkube to start master, node, and gateway services"
	c.Version = config.Version
	c.CustomAppHelpTemplate = CustomAppHelpTemplate
	c.Flags = config.GetFlags(nil)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	c.Action = func(ctx *cli.Context) error {
		return startOvnKube(ctx, cancel)
	}

	// trap SIGHUP, SIGINT, SIGTERM, SIGQUIT and
	// cancel the context
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer func() {
		signal.Stop(exitCh)
		cancel()
	}()
	go func() {
		select {
		case s := <-exitCh:
			klog.Infof("Received signal %s. Shutting down", s)
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := c.RunContext(ctx, os.Args); err != nil {
		klog.Exit(err)
	}
}

func delPidfile(pidfile string) {
	if pidfile != "" {
		if _, err := os.Stat(pidfile); err == nil {
			if err := os.Remove(pidfile); err != nil {
				klog.Errorf("%s delete failed: %v", pidfile, err)
			}
		}
	}
}

func setupPIDFile(pidfile string) error {
	// need to test if already there
	_, err := os.Stat(pidfile)

	// Create if it doesn't exist, else exit with error
	if os.IsNotExist(err) {
		if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
			klog.Errorf("Failed to write pidfile %s (%v). Ignoring..", pidfile, err)
		}
	} else {
		// get the pid and see if it exists
		pid, err := os.ReadFile(pidfile)
		if err != nil {
			return fmt.Errorf("pidfile %s exists but can't be read: %v", pidfile, err)
		}
		_, err1 := os.Stat("/proc/" + string(pid[:]) + "/cmdline")
		if os.IsNotExist(err1) {
			// Left over pid from dead process
			if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
				klog.Errorf("Failed to write pidfile %s (%v). Ignoring..", pidfile, err)
			}
		} else {
			return fmt.Errorf("pidfile %s exists and ovnkube is running", pidfile)
		}
	}

	return nil
}

// ovnkubeRunMode object stores the run mode of the ovnkube
type ovnkubeRunMode struct {
	ovnkubeController bool // ovnkube controller (--init-ovnkube-controller or --init-master) is enabled
	clusterManager    bool // cluster manager (--init-cluster-manager or --init-master) is enabled
	node              bool // node (--init-node) is enabled
	cleanupNode       bool // cleanup (--cleanup-node) is enabled

	// Along with the run mode, an identity is provided that uniquely identifies
	// this instance vs other instances that might be running in the cluster.
	// The identity is usually the node name. It's used for leader election
	// among other things.
	identity string
}

// determineOvnkubeRunMode determines the run modes of ovnkube
// based on the init flags set.  It is possible to run ovnkube in
// multiple modes.  Allowed multiple modes are:
//   - master (ovnkube controller + cluster manager) + node
//   - ovnkube controller + cluster manager
//   - ovnkube controller + node
func determineOvnkubeRunMode(ctx *cli.Context) (*ovnkubeRunMode, error) {
	mode := &ovnkubeRunMode{}

	master := ctx.String("init-master")
	cm := ctx.String("init-cluster-manager")
	ovnkController := ctx.String("init-ovnkube-controller")
	node := ctx.String("init-node")
	cleanup := ctx.String("cleanup-node")

	if master != "" {
		// If init-master is set, then both ovnkube controller and cluster manager
		// are enabled
		mode.ovnkubeController = true
		mode.clusterManager = true
	}

	if cm != "" {
		mode.clusterManager = true
	}

	if ovnkController != "" {
		mode.ovnkubeController = true
	}

	if node != "" {
		mode.node = true
	}

	if cleanup != "" {
		mode.cleanupNode = true
	}

	if mode.cleanupNode && (mode.clusterManager || mode.ovnkubeController || mode.node) {
		return nil, fmt.Errorf("cannot run cleanup-node mode along with any other mode")
	}

	if !mode.clusterManager && !mode.ovnkubeController && !mode.node && !mode.cleanupNode {
		return nil, fmt.Errorf("need to specify a mode for ovnkube")
	}

	if !mode.ovnkubeController && mode.clusterManager && mode.node {
		return nil, fmt.Errorf("cannot run in both cluster manager and node mode")
	}

	identities := sets.NewString(master, cm, ovnkController, node, cleanup)
	identities.Delete("")
	if identities.Len() != 1 {
		return nil, fmt.Errorf("provided no identity or different identities for different modes")
	}

	mode.identity, _ = identities.PopAny()
	// OCP HACK: when cluster manager runs alone, use pod name as identity to avoid duplicate leaders
	// while switching from global zone to multizone
	if mode.clusterManager && !mode.ovnkubeController {
		podName, hasPodName := os.LookupEnv("POD_NAME")
		if hasPodName {
			mode.identity = podName
		}
	}
	// END OCP HACK
	return mode, nil
}

func startOvnKube(ctx *cli.Context, cancel context.CancelFunc) error {
	pidfile := ctx.String("pidfile")
	if pidfile != "" {
		defer delPidfile(pidfile)
		if err := setupPIDFile(pidfile); err != nil {
			return err
		}
	}

	exec := kexec.New()
	_, err := config.InitConfig(ctx, exec, nil)
	if err != nil {
		return err
	}

	if err = util.SetExec(exec); err != nil {
		return fmt.Errorf("failed to initialize exec helper: %v", err)
	}

	ovnKubeStartWg := &sync.WaitGroup{}
	defer func() {
		// make sure everything stops and wait
		cancel()
		ovnKubeStartWg.Wait()
	}()

	if config.Kubernetes.BootstrapKubeconfig != "" {
		if err := util.StartNodeCertificateManager(ctx.Context, ovnKubeStartWg, os.Getenv("K8S_NODE"), &config.Kubernetes); err != nil {
			return fmt.Errorf("failed to start the node certificate manager: %w", err)
		}
	}
	ovnClientset, err := util.NewOVNClientset(&config.Kubernetes)
	if err != nil {
		return err
	}

	runMode, err := determineOvnkubeRunMode(ctx)
	if err != nil {
		return err
	}

	eventRecorder := util.EventRecorder(ovnClientset.KubeClient)

	// Start metric server for master and node. Expose the metrics HTTP endpoint if configured.
	// Non LE master instances also are required to expose the metrics server.
	if config.Metrics.BindAddress != "" {
		metrics.StartMetricsServer(config.Metrics.BindAddress, config.Metrics.EnablePprof,
			config.Metrics.NodeServerCert, config.Metrics.NodeServerPrivKey, ctx.Done(), ovnKubeStartWg)
	}

	// no need for leader election in node mode
	// only node mode
	if !runMode.clusterManager && !runMode.ovnkubeController {
		return runOvnKube(ctx.Context, runMode, ovnClientset, eventRecorder)
	}

	// ovnkube-controller with node
	if runMode.node && runMode.ovnkubeController {
		metrics.RegisterOVNKubeControllerBase()
		return runOvnKube(ctx.Context, runMode, ovnClientset, eventRecorder)
	}

	// Register prometheus metrics that do not depend on becoming ovnkube-controller
	// leader and get the proper HA config depending on the mode. For ovnkube
	// controller mode or combined cluster manager and ovnkube-controller modes (the classic
	// master mode), the master HA config applies. For cluster manager
	// standalone mode, the cluster manager HA config applies.
	var haConfig *config.HAConfig
	var name string
	switch {
	case runMode.ovnkubeController && runMode.clusterManager:
		metrics.RegisterClusterManagerBase()
		fallthrough
	case runMode.ovnkubeController:
		metrics.RegisterOVNKubeControllerBase()
		haConfig = &config.MasterHA
		name = networkControllerManagerLockName()
	case runMode.clusterManager:
		metrics.RegisterClusterManagerBase()
		haConfig = &config.ClusterMgrHA
		name = "ovn-kubernetes-master"
	}

	// Set up leader election process. Use lease resource lock as configmap and
	// endpoint lock support has been removed from leader election library.
	rl, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		name,
		ovnClientset.KubeClient.CoreV1(),
		ovnClientset.KubeClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      runMode.identity,
			EventRecorder: eventRecorder,
		},
	)
	if err != nil {
		return err
	}

	ovnKubeStopped := false
	ovnKubeStopLock := sync.Mutex{}
	lec := leaderelection.LeaderElectionConfig{
		Lock:            rl,
		LeaseDuration:   time.Duration(haConfig.ElectionLeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(haConfig.ElectionRenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(haConfig.ElectionRetryPeriod) * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// Looking at the leader elector implementation,
				// OnStartedLeading is called asynchronously with respect
				// OnStoppedLeading and there seems to be no guarantee that
				// OnStartedLeading won't run after OnStoppedLeading, so take
				// some additional precautions to ensure we don't start when we
				// shouldn't.
				ovnKubeStopLock.Lock()
				if ovnKubeStopped {
					ovnKubeStopLock.Unlock()
					return
				}
				ovnKubeStartWg.Add(1)
				defer ovnKubeStartWg.Done()
				ovnKubeStopLock.Unlock()
				klog.Infof("Won leader election; in active mode")
				if err := runOvnKube(ctx, runMode, ovnClientset, eventRecorder); err != nil {
					klog.Error(err)
					cancel()
				}
			},
			OnStoppedLeading: func() {
				ovnKubeStopLock.Lock()
				defer ovnKubeStopLock.Unlock()
				klog.Infof("No longer leader; exiting")
				ovnKubeStopped = true
				cancel()
			},
			OnNewLeader: func(newLeaderName string) {
				if newLeaderName != runMode.identity {
					klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
				}
			},
		},
	}

	leaderelection.SetProvider(ovnkubeMetricsProvider{runMode})
	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	leaderElector.Run(ctx.Context)
	// Looking at the leader election implementation, OnStoppedLeading is called
	// synchronously before Run exits. But the callbacks are documented as
	// asynchronous so again out of precaution we make sure we don't start when
	// we shouldn't.
	ovnKubeStopLock.Lock()
	ovnKubeStopped = true
	ovnKubeStopLock.Unlock()

	return nil
}

func runOvnKube(ctx context.Context, runMode *ovnkubeRunMode, ovnClientset *util.OVNClientset, eventRecorder record.EventRecorder) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic in runOvnKube: %v", r)
		}
	}()
	startTime := time.Now()

	if runMode.cleanupNode {
		return ovnnode.CleanupClusterNode(runMode.identity)
	}

	stopChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	defer func() {
		close(stopChan)
		wg.Wait()
	}()

	var masterWatchFactory *factory.WatchFactory

	if runMode.ovnkubeController {
		// create factory and start the controllers asked for
		masterWatchFactory, err = factory.NewOVNKubeControllerWatchFactory(ovnClientset.GetOVNKubeControllerClientset())
		if err != nil {
			return err
		}
		defer masterWatchFactory.Shutdown()
	}

	if runMode.clusterManager {
		var clusterManagerWatchFactory *factory.WatchFactory
		if runMode.ovnkubeController {
			// if CM and NCM modes are enabled, then we should call the combo mode - NewMasterWatchFactory
			masterWatchFactory, err = factory.NewMasterWatchFactory(ovnClientset.GetMasterClientset())
			if err != nil {
				return err
			}
			clusterManagerWatchFactory = masterWatchFactory
		} else {
			clusterManagerWatchFactory, err = factory.NewClusterManagerWatchFactory(ovnClientset.GetClusterManagerClientset())
			if err != nil {
				return err
			}
			defer clusterManagerWatchFactory.Shutdown()
		}

		clusterManager, err := clustermanager.NewClusterManager(ovnClientset.GetClusterManagerClientset(), clusterManagerWatchFactory,
			runMode.identity, wg, eventRecorder)
		if err != nil {
			return fmt.Errorf("failed to create new cluster manager: %w", err)
		}
		metrics.RegisterClusterManagerFunctional()
		err = clusterManager.Start(ctx)
		if err != nil {
			return fmt.Errorf("failed to start cluster manager: %w", err)
		}
		defer clusterManager.Stop()

		// record delay until ready
		metrics.MetricClusterManagerReadyDuration.Set(time.Since(startTime).Seconds())
	}

	var ovnkubeControllerStartErr error
	ovnkubeControllerWG := sync.WaitGroup{}
	if runMode.ovnkubeController {
		var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client

		if libovsdbOvnNBClient, err = libovsdb.NewNBClient(stopChan); err != nil {
			return fmt.Errorf("error when trying to initialize libovsdb NB client: %v", err)
		}

		if libovsdbOvnSBClient, err = libovsdb.NewSBClient(stopChan); err != nil {
			return fmt.Errorf("error when trying to initialize libovsdb SB client: %v", err)
		}

		networkControllerManager, err := controllerManager.NewNetworkControllerManager(ovnClientset, masterWatchFactory, libovsdbOvnNBClient, libovsdbOvnSBClient, eventRecorder, wg)
		if err != nil {
			return err
		}

		// start NetworkControllerManager in a separate goroutine to allow parallel startup for NodeNetworkControllerManager.
		// NetworkControllerManager during startup waits for ovnkube-node to set ovnNodeZoneName annotation, therefore
		// they can't run sequentially (unless we use default "global" zone).
		// Another advantage of running startup in parallel is reducing the startup time.
		ovnkubeControllerWG.Add(1)
		go func() {
			defer ovnkubeControllerWG.Done()
			err = networkControllerManager.Start(ctx)
			if err != nil {
				ovnkubeControllerStartErr = fmt.Errorf("failed to start ovnkube controller: %w", err)
				klog.Error(ovnkubeControllerStartErr)
				return
			}
			// record delay until ready
			metrics.MetricOVNKubeControllerReadyDuration.Set(time.Since(startTime).Seconds())
		}()
		// make sure ovnkubeController started in a separate goroutine will execute .Stop() on shutdown.
		// Stop() only makes sense to call if Start() succeeded.
		defer func() {
			ovnkubeControllerWG.Wait()
			if ovnkubeControllerStartErr == nil {
				networkControllerManager.Stop()
			}
		}()
	}

	if runMode.node {
		var nodeWatchFactory factory.NodeWatchFactory

		if runMode.ovnkubeController {
			// masterWatchFactory would be initialized as NewOVNKubeControllerWatchFactory already, let's use that
			nodeWatchFactory = masterWatchFactory
		} else {
			var err error
			nodeWatchFactory, err = factory.NewNodeWatchFactory(ovnClientset.GetNodeClientset(), runMode.identity)
			if err != nil {
				return err
			}
			defer nodeWatchFactory.Shutdown()
		}

		if config.Kubernetes.Token == "" {
			return fmt.Errorf("cannot initialize node without service account 'token'. Please provide one with --k8s-token argument")
		}
		// register ovnkube node specific prometheus metrics exported by the node
		metrics.RegisterNodeMetrics()
		ncm, err := controllerManager.NewNodeNetworkControllerManager(ovnClientset, nodeWatchFactory, runMode.identity, eventRecorder)
		if err != nil {
			return fmt.Errorf("failed to create ovnkube node ovnkube controller: %w", err)
		}
		err = ncm.Start(ctx)
		if err != nil {
			return fmt.Errorf("failed to start node network manager: %w", err)
		}
		defer ncm.Stop()

		// record delay until ready
		metrics.MetricNodeReadyDuration.Set(time.Since(startTime).Seconds())
	}

	// wait for ovnkubeController to start and check error
	if runMode.ovnkubeController {
		ovnkubeControllerWG.Wait()
		if ovnkubeControllerStartErr != nil {
			return ovnkubeControllerStartErr
		}
	}

	// start the prometheus server to serve OVS and OVN Metrics (default port: 9476)
	// Note: for ovnkube node mode dpu-host no metrics is required as ovs/ovn is not running on the node.
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost && config.Metrics.OVNMetricsBindAddress != "" {
		if config.Metrics.ExportOVSMetrics {
			metrics.RegisterOvsMetricsWithOvnMetrics(stopChan)
		}
		metrics.RegisterOvnMetrics(ovnClientset.KubeClient, runMode.identity, stopChan)
		metrics.StartOVNMetricsServer(config.Metrics.OVNMetricsBindAddress,
			config.Metrics.NodeServerCert, config.Metrics.NodeServerPrivKey, stopChan, wg)
	}

	// run until cancelled
	<-ctx.Done()
	return nil
}

type leaderMetrics struct {
	runMode *ovnkubeRunMode
}

func (m leaderMetrics) On(string) {
	if m.runMode.ovnkubeController {
		metrics.MetricOVNKubeControllerLeader.Set(1)
	}
	if m.runMode.clusterManager {
		metrics.MetricClusterManagerLeader.Set(1)
	}
}

func (m leaderMetrics) Off(string) {
	if m.runMode.ovnkubeController {
		metrics.MetricOVNKubeControllerLeader.Set(0)
	}
	if m.runMode.clusterManager {
		metrics.MetricClusterManagerLeader.Set(0)
	}
}

type ovnkubeMetricsProvider struct {
	runMode *ovnkubeRunMode
}

func (p ovnkubeMetricsProvider) NewLeaderMetric() leaderelection.SwitchMetric {
	return &leaderMetrics{p.runMode}
}

func networkControllerManagerLockName() string {
	// keep the same old lock name unless we are owners of a specific zone
	name := "ovn-kubernetes-master"
	if config.Default.Zone != types.OvnDefaultZone {
		name = name + "-" + config.Default.Zone
	}
	return name
}
