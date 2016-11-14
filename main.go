/* Copyright 2015 LinkedIn Corp. Licensed under the Apache License, Version
 * 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 */

package main

import (
	"flag"
	"fmt"
	log "github.com/cihub/seelog"
	"github.com/linkedin/Burrow/protocol"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuel/go-zookeeper/zk"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
        "strings"
        "regexp"
        "strconv"
)

type KafkaCluster struct {
	Client    *KafkaClient
	Zookeeper *ZookeeperClient
}

type StormCluster struct {
	Storm *StormClient
}

type ApplicationContext struct {
	Config       *BurrowConfig
	Storage      *OffsetStorage
	Clusters     map[string]*KafkaCluster
	Storms       map[string]*StormCluster
	Server       *HttpServer
	NotifyCenter *NotifyCenter
	NotifierLock *zk.Lock
        GaugeMetrics map[string]*prometheus.GaugeVec
}

// Why two mains? Golang doesn't let main() return, which means defers will not run.
// So we do everything in a separate main, that way we can easily exit out with an error code and still run defers
func burrowMain() int {
	// The only command line arg is the config file
	var cfgfile = flag.String("config", "burrow.cfg", "Full path to the configuration file")
	flag.Parse()

	// Load and validate the configuration
	fmt.Fprintln(os.Stderr, "Reading configuration from", *cfgfile)
	appContext := &ApplicationContext{Config: ReadConfig(*cfgfile)}
	if err := ValidateConfig(appContext); err != nil {
		log.Criticalf("Cannot validate configuration: %v", err)
		return 1
	}

	// Create the PID file to lock out other processes. Defer removal so it's the last thing to go
	createPidFile(appContext.Config.General.LogDir + "/" + appContext.Config.General.PIDFile)
	defer removePidFile(appContext.Config.General.LogDir + "/" + appContext.Config.General.PIDFile)

	// Set up stderr/stdout to go to a separate log file
	openOutLog(appContext.Config.General.LogDir + "/burrow.out")
	fmt.Println("Started Burrow at", time.Now().Format("January 2, 2006 at 3:04pm (MST)"))

	// If a logging config is specified, replace the existing loggers
	if appContext.Config.General.LogConfig != "" {
		NewLogger(appContext.Config.General.LogConfig)
	}

	// Start a local Zookeeper client (used for application locks)
	log.Info("Starting Zookeeper client")
	zkconn, _, err := zk.Connect(appContext.Config.Zookeeper.Hosts, time.Duration(appContext.Config.Zookeeper.Timeout)*time.Second)
	if err != nil {
		log.Criticalf("Cannot start Zookeeper client: %v", err)
		return 1
	}
	defer zkconn.Close()

	// Start an offsets storage module
	log.Info("Starting Offsets Storage module")
	appContext.Storage, err = NewOffsetStorage(appContext)
	if err != nil {
		log.Criticalf("Cannot configure offsets storage module: %v", err)
		return 1
	}
	defer appContext.Storage.Stop()

	// Start an HTTP server
	log.Info("Starting HTTP server")
	appContext.Server, err = NewHttpServer(appContext)
	if err != nil {
		log.Criticalf("Cannot start HTTP server: %v", err)
		return 1
	}
	defer appContext.Server.Stop()

	// Start Kafka clients and Zookeepers for each cluster
	appContext.Clusters = make(map[string]*KafkaCluster, len(appContext.Config.Kafka))
	for cluster, _ := range appContext.Config.Kafka {
		log.Infof("Starting Zookeeper client for cluster %s", cluster)
		zkconn, err := NewZookeeperClient(appContext, cluster)
		if err != nil {
			log.Criticalf("Cannot start Zookeeper client for cluster %s: %v", cluster, err)
			return 1
		}
		defer zkconn.Stop()

		log.Infof("Starting Kafka client for cluster %s", cluster)
		client, err := NewKafkaClient(appContext, cluster)
		if err != nil {
			log.Criticalf("Cannot start Kafka client for cluster %s: %v", cluster, err)
			return 1
		}
		defer client.Stop()

		appContext.Clusters[cluster] = &KafkaCluster{Client: client, Zookeeper: zkconn}
	}

	// Start Storm Clients for each storm cluster
	appContext.Storms = make(map[string]*StormCluster, len(appContext.Config.Storm))
	for cluster, _ := range appContext.Config.Storm {
		log.Infof("Starting Storm client for cluster %s", cluster)
		stormClient, err := NewStormClient(appContext, cluster)
		if err != nil {
			log.Criticalf("Cannot start Storm client for cluster %s: %v", cluster, err)
			return 1
		}
		defer stormClient.Stop()

		appContext.Storms[cluster] = &StormCluster{Storm: stormClient}
	}

	// Set up the Zookeeper lock for notification
	appContext.NotifierLock = zk.NewLock(zkconn, appContext.Config.Zookeeper.LockPath, zk.WorldACL(zk.PermAll))

	// Load the notifiers, but do not start them
	err = LoadNotifiers(appContext)
	if err != nil {
		// Error was already logged
		return 1
	}

	// Notifiers are started in a goroutine if we get the ZK lock
	go StartNotifiers(appContext)
	defer StopNotifiers(appContext)

	appContext.GaugeMetrics = make(map[string]*prometheus.GaugeVec)
	go appContext.prometheusUpdater()

	// Register signal handlers for exiting
	exitChannel := make(chan os.Signal, 1)
	signal.Notify(exitChannel, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGSTOP, syscall.SIGTERM)

	// Wait until we're told to exit
	<-exitChannel
	log.Info("Shutdown triggered")
	return 0
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	rv := burrowMain()
	if rv != 0 {
		fmt.Println("Burrow failed at", time.Now().Format("January 2, 2006 at 3:04pm (MST)"))
	} else {
		fmt.Println("Stopped Burrow at", time.Now().Format("January 2, 2006 at 3:04pm (MST)"))
	}
	os.Exit(rv)
}

func (app *ApplicationContext) prometheusUpdater() {
	for {
		<-time.After(5 * time.Second)
		app.UpdatePrometheusMetrics()
	}
}

func (ctx *ApplicationContext) GetOrCreateGauge(gaugeName string, gaugeHelp string, labels []string) *prometheus.GaugeVec {
	metric := ctx.GaugeMetrics[gaugeName]
	if metric != nil {
		return metric
	}
	gaugeOpts := prometheus.GaugeOpts{
		Name: gaugeName,
		Help: gaugeHelp,
	}
	gauge := prometheus.NewGaugeVec(
		gaugeOpts,
		labels,
	)
	prometheus.Register(gauge)
	ctx.GaugeMetrics[gaugeName] = gauge
	return gauge
}

func (app *ApplicationContext) UpdatePrometheusMetrics() {
	r := regexp.MustCompile("[^a-zA-Z0-9_]+")

	lagGauge := app.GetOrCreateGauge("kafka_lag", "Gauge of lag (messages produces - messages consumed) for a Kafka consumer group", []string{"cluster", "consumer_group", "partition", "topic"})
	totalLagGauge := app.GetOrCreateGauge("kafka_lag_total", "Gauge of total lag (messages produces - messages consumed) for a Kafka consumer group", []string{"cluster", "consumer_group"})
	offsetGauge := app.GetOrCreateGauge("kafka_offset", "Gauge of offset for a Kafka consumer group", []string{"cluster", "consumer_group", "partition", "topic"})

	for cluster, _ := range app.Config.Kafka {
		consumers := getConsumerList(app, cluster)
		for _, consumer := range consumers {

			consumerStat := getConsumerStatus(app, cluster, consumer)

			// Make the metric/label names acceptable for prometheus
			clusterProm := strings.Replace(cluster, "-", "_", -1)
			clusterProm = r.ReplaceAllString(clusterProm, "")

			cgNameProm := strings.Replace(consumer, "-", "_", -1)
			cgNameProm = r.ReplaceAllString(cgNameProm, "")

			totalLagGauge.WithLabelValues(clusterProm, cgNameProm).Set(float64(consumerStat.TotalLag))

			for _, partition := range consumerStat.Partitions {
				lagGauge.WithLabelValues(clusterProm, cgNameProm, strconv.FormatInt(int64(partition.Partition), 10), partition.Topic).Set(float64(partition.End.Lag))
				offsetGauge.WithLabelValues(clusterProm, cgNameProm, strconv.FormatInt(int64(partition.Partition), 10), partition.Topic).Set(float64(partition.End.Offset))
			}
		}
	}
}

func getConsumerList(app *ApplicationContext, cluster string) []string {
	storageRequest := &RequestConsumerList{Result: make(chan []string), Cluster: cluster}
	app.Storage.requestChannel <- storageRequest
	select {
	case res := <-storageRequest.Result:
		return res
	case <-time.After(10 * time.Second):
		log.Warn("Timed out after 10 seconds for consumer list response")
		return []string{}
	}
}

func getConsumerStatus(app *ApplicationContext, cluster string, group string) *protocol.ConsumerGroupStatus {
	storageRequest := &RequestConsumerStatus{
		Result:  make(chan *protocol.ConsumerGroupStatus),
		Cluster: cluster,
		Group:   group,
		Showall: true,
	}
	app.Storage.requestChannel <- storageRequest
	select {
	case res := <-storageRequest.Result:
		return res
	case <-time.After(10 * time.Second):
		log.Warn("Timed out after 10 seconds for consumer status response")
		return nil
	}
}