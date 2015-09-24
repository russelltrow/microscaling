// Force12.io is a package that monitors demand for resource in a system and then scales and repurposes
// containers, based on agreed "quality of service" contracts, to best handle that demand within the constraints of your existing VM
// or physical infrastructure (for v1).
//
// Force12 is defined to optimize the use of existing physical and VM resources instantly. VMs cannot be scaled in real time (it takes
// several minutes) and new physical machines take even longer. However, containers can be started or stopped at sub second speeds,
// allowing your infrastructure to adapt itself in real time to meet system demands.
//
// Force12 is aimed at effectively using the resources you have right now - your existing VMs or physical servers - by using them as
// optimally as possible.
//
// The Force12 approach is analogous to the way that a router dynamically optimises the use of a physical network. A router is limited
// by the capacity of the lines physically connected to it. Adding additional capacity is a physical process and takes time. Routers
// therefore make decisions in real time about which packets will be prioritized on a particular line based on the packet's priority
// (defined by a "quality of service" contract).
//
// For example, at times of high bandwidth usage a router might prioritize VOIP traffic over web browsing in real time.
//
// Containers allow Force12 to make similar "instant" judgements on service prioritisation within your existing infrastructure. Routers
// make very simplistic judgments because they have limited time and cpu and they act at a per packet level. Force12 has the capability
// of making far more sophisticated judgements, although even fairly simple ones will still provide a significant new service.
//
// This prototype is a bare bones implementation of Force12.io that recognises only 1 demand type:
// randomised demand for a priority 1 service. Resources are allocated to meet this demand for priority 1, and spare resource can
// be used for a priority 2 service.
//
// These demand type examples have been chosen purely for simplicity of demonstration. In the future more demand types
// will be offered
//
// V1 - Force12.io reacts to increased demand by starting/stopping containers on the slaves already in play.
//
// This version of Force12 starts and stops containers on a Mesos cluser using Marathon as the scheduler
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"bitbucket.org/force12io/force12-scheduler/compose"
	"bitbucket.org/force12io/force12-scheduler/consul"
	"bitbucket.org/force12io/force12-scheduler/demand"
	"bitbucket.org/force12io/force12-scheduler/marathon"
	"bitbucket.org/force12io/force12-scheduler/rng"
	"bitbucket.org/force12io/force12-scheduler/scheduler"
)

type sendStatePayload struct {
	CreatedAt          int64 `json:"createdAt"`
	Priority1Requested int   `json:"priority1Requested"`
	Priority1Running   int   `json:"priority1Running"`
	Priority2Running   int   `json:"priority2Running"`
}

const const_sleep = 100          //milliseconds
const const_sendstate_sleeps = 5 // number of sleeps before we send state on the API
const const_stopsleep = 250      //milliseconds pause between stopping and restarting containers
const const_p1demandstart int = 5
const const_p2demandstart int = 4
const const_maxcontainers int = 9

var p1TaskName string
var p2TaskName string
var p1FamilyName string
var p2FamilyName string

type Demand struct {
	sched scheduler.Scheduler
	input demand.Input

	p1demand    int // number of Priority 1 tasks demanded
	p2demand    int
	p1requested int // indicates how many P1 tasks we've tried to kick off.
	p2requested int
}

// set returns values that were there (p1, p2)
// if provided value is -1 don't update, demand will always be between 0 and const_maxcontainers
func (d *Demand) set(p1, p2 int) (int, int) {
	//d.mu.Lock()
	p1old := d.p1demand
	p2old := d.p2demand
	if p2 != -1 {
		d.p2demand = p2
	}
	if p1 != -1 {
		d.p1demand = p1
	}
	//d.mu.Unlock()
	return p1old, p2old
}

// get returns client, server AEC - Combine this with the set to reduce code
func (d *Demand) get() (int, int) {
	return d.p1demand, d.p2demand
}

// handle processes a change in demand
// Note that handle will make any judgment on what to do with a demand
// change, including potentially nothing.
func (d *Demand) handle() error {
	var err error
	err = d.sched.StopStartNTasks(p1TaskName, p1FamilyName, d.p1demand, d.p1requested)
	if err != nil {
		log.Printf("Failed to start Priority1 tasks. %v", err)
	}
	d.sched.StopStartNTasks(p2TaskName, p2FamilyName, d.p2demand, d.p2requested)
	if err != nil {
		log.Printf("Failed to start Priority2 tasks. %v", err)
	}

	return err
}

// update checks for changes in demand, returning true if demand changed
// Note that this function makes no judgement on whether a demand change is
// significant. handle() will determine that.
func (d *Demand) update() bool {
	//log.Println("demand update check.")
	var demandchange bool

	newP1Demand, err := d.input.GetDemand("priority1-demand")
	if err != nil {
		log.Printf("Failed to get new demand. %v", err)
		return false
	}
	//log.Printf("container count %v\n", container_count)
	newP2Demand := const_maxcontainers - newP1Demand

	//Update our saved demand
	oldP1Demand, oldP2Demand := d.set(newP1Demand, newP2Demand)

	//Has the demand changed?
	demandchange = (newP1Demand != oldP1Demand) || (newP2Demand != oldP2Demand)

	if demandchange {
		log.Printf("P1 demand changed from %d to %d", oldP1Demand, newP1Demand)
	}

	return demandchange
}

// sendStateToAPI checks the current state of cluster (or single node) and sends that
// state to the f12 API
func sendStateToAPI(currentdemand *Demand) error {
	count1, count2, err := currentdemand.sched.CountAllTasks()
	if err != nil {
		return fmt.Errorf("Failed to get state err %v", err)
	}

	// Submit a PUT request to the API
	// Note the magic hardcoded string is the user ID, we need to pass this in in some way. ENV VAR?
	url := getBaseF12APIUrl() + "/metrics/" + "5k5gk"
	log.Printf("API PUT: %s", url)

	payload := sendStatePayload{
		CreatedAt:          time.Now().Unix(),
		Priority1Requested: currentdemand.p1demand,
		Priority1Running:   count1,
		Priority2Running:   count2,
	}

	w := &bytes.Buffer{}
	encoder := json.NewEncoder(w)
	err = encoder.Encode(&payload)
	if err != nil {
		return fmt.Errorf("Failed to encode API json. %v", err)
	}

	req, err := http.NewRequest("PUT", url, w)

	if err != nil {
		return fmt.Errorf("Failed to build API PUT request err %v", err)
	}
	//req.Header.Set("X-Custom-Header", "myvalue")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	defer resp.Body.Close()

	if err != nil {
		// handle error
		return fmt.Errorf("API state err %v", err)
	}

	if resp.StatusCode > 204 {
		return fmt.Errorf("error response from API. %s", resp.Status)
	}
	return err
}

func getBaseF12APIUrl() string {
	baseUrl := os.Getenv("API_ADDRESS")
	if baseUrl == "" {
		baseUrl = "https://force12-windtunnel.herokuapp.com"
	}
	return baseUrl
}

func getEnvOrDefault(name string, defaultValue string) string {
	v := os.Getenv(name)
	if v == "" {
		v = defaultValue
	}

	return v
}

// For the simple prototype, Force12.io sits in a loop checking for demand changes every X milliseconds
// In phase 2 we'll add a reactive mode where appropriate.
//
// Note - we don't route messages from demandcheckers to demandhandlers using channels because we want new values
// to override old values. Queued history is of no importance here.
//
// Also for simplicity this first release is concurrency free (single threaded)
func main() {
	var err error
	// TODO! Make it so you can send in settings on the command line
	var demandModelType string = getEnvOrDefault("F12_DEMAND_MODEL", "RNG")
	var schedulerType string = getEnvOrDefault("F12_SCHEDULER", "COMPOSE")
	var sendstate string = getEnvOrDefault("F12_SEND_STATE_TO_API", "true")
	p1TaskName = getEnvOrDefault("F12_PRIORITY1_TASK", "priority1-demand")
	p2TaskName = getEnvOrDefault("F12_PRIORITY2_TASK", "priority2-demand")
	// TODO!! FInd out what CLIENT/SERVER_FAMILY should default to
	p1FamilyName = os.Getenv("F12_PRIORITY1_FAMILY")
	p2FamilyName = os.Getenv("F12_PRIORITY2_FAMILY")

	var di demand.Input
	var s scheduler.Scheduler

	switch demandModelType {
	case "CONSUL":
		log.Println("Getting demand metric from Consul")
		di = consul.NewDemandModel()
	case "RNG":
		log.Println("Random demand generation")
		di = rng.NewDemandModel()
	default:
		log.Printf("Bad value for F12_DEMAND_MODEL: %s", demandModelType)
		return
	}

	switch schedulerType {
	case "MESOS":
		log.Println("Scheduling with Mesos / Marathon")
		s = marathon.NewScheduler()
	case "COMPOSE":
		log.Println("Scheduling with Docker compose")
		s = compose.NewScheduler()
	default:
		log.Printf("Bad value for F12_SCHEDULER: %s", schedulerType)
		return
	}

	currentdemand := Demand{
		input: di,
	}
	currentdemand.set(const_p1demandstart, const_p2demandstart)

	// Initialise container types
	err = currentdemand.sched.InitScheduler(p1TaskName)
	if err != nil {
		log.Printf("Failed to start P1 task. %v", err)
		return
	}

	err = currentdemand.sched.InitScheduler(p2TaskName)
	if err != nil {
		log.Printf("Failed to start P2 task. %v", err)
		return
	}

	var demandchangeflag bool
	demandchangeflag = currentdemand.update()
	demandchangeflag = true

	var sleepcount int = 0
	var sleep time.Duration
	sleep = const_sleep * time.Millisecond

	for {
		//Update currentdemand with latest client and server demand, if changed, set flag
		demandchangeflag = currentdemand.update()
		if demandchangeflag {
			// See how many tasks we should have already
			currentdemand.p1requested, currentdemand.p2requested, err = currentdemand.sched.CountAllTasks()
			if err != nil {
				log.Printf("Failed to count tasks. %v", err)
			}
			//make any changes dictated by the new demand level
			err = currentdemand.handle()
			if err != nil {
				log.Printf("Failed to handle demand change. %v", err)
			}
		}

		time.Sleep(sleep)
		sleepcount++
		if sleepcount == const_sendstate_sleeps {
			sleepcount = 0

			//Periodically send state to the API if required
			if sendstate == "true" {
				err = sendStateToAPI(&currentdemand)
				if err != nil {
					log.Printf("Failed to send state. %v", err)
				}
			}
		}

	}
}
