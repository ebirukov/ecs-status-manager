package main

import (
	fmt "fmt"
	"github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/events"
	"golang.org/x/net/context"
	"encoding/json"
	"log"
	"io"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"net/http"
	"time"
	"strings"
	"errors"
	"github.com/docker/engine-api/types/filters"
	"strconv"
	"flag"
	"os"
)

type TaskInfo struct {
	Arn string
	DesiredStatus string
	KnownStatus string
	Family string
	Containers []struct {
		DockerId string
		DockerName string
		Name string
	}
}

type ECSContainerInfo struct {
	Id string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
	Cluster string `json:"cluster,omitempty"`
	Image string `json:"image,omitempty"`
	Task string `json:"task,omitempty"`
	ExitCode int `json:"exitCode,string,omitempty"`
	Time string `json:"time,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
}

var httpClient = &http.Client{Timeout: 1 * time.Second}
var tasks = make(map[string]TaskInfo)

// this is a comment
func main() {
	arn := flag.String("arn", os.Getenv("ARN"), "sns arn for publish events")
	region := flag.String("region", "us-east-1", "aws sns region")
	clusterName := flag.String("cn", os.Getenv("CLUSTER_NAME"), "ecs cluster name")
	if *arn == "" {
		*arn = "arn:aws:sns:us-east-1:560230448151:notifyMe"
	}
	flag.Parse()
	defaultHeaders := map[string]string{"User-Agent": "engine-api-cli-1.0"}
	for {
		cli, err := client.NewClient("unix:///var/run/docker.sock", "v1.24", nil, defaultHeaders)
		if err != nil {
			panic(err)
		}
		statusFilter := filters.NewArgs()
		statusFilter.Add("status", "created")
		statusFilter.Add("status", "running")
		options := types.ContainerListOptions{All: true, Filter: statusFilter}
		containers, err := cli.ContainerList(context.Background(), options)
		if err != nil {
			panic(err)
		}

		for _, c := range containers {
			info, err := cli.ContainerInspect(context.Background(), c.ID)
			if err != nil {
				log.Println(err)
				continue
			}
			log.Println(" state ", info.State)
			task, err := getTaskInfo(c.ID)
			if err != nil {
				log.Println(c.ID, err)
				continue
			}
			log.Println(task)
		}
		eventTypeFilter := filters.NewArgs()
		eventTypeFilter.Add("type", "container")
		body, err := cli.Events(context.Background(), types.EventsOptions{Filters: eventTypeFilter})
		if err != nil {
			log.Fatal(err)
		}
		awsConfig := &aws.Config{Region: region}
		session, err := session.NewSession(awsConfig)
		if err != nil {
			log.Fatal(err)
		}
		svc := sns.New(session)

		dec := json.NewDecoder(body)
		for {
			var event events.Message
			err := dec.Decode(&event)
			if err != nil && err == io.EOF {
				break
			}
			if event.ID == "" {
				break
			}
			if event.Action == "destroy" {
				delete(tasks, event.ID)
				log.Print("tasks size ", len(tasks))
				continue
			}
			task, err := getTaskInfo(event.ID)
			if err != nil {
				log.Println(event.ID, err)
				continue
			}
			containerInfo := ECSContainerInfo{Id: strings.Split(task.Arn, "/")[1], Status: calcStatus(event.Action), Image: event.From, Task: task.Containers[0].DockerName, Cluster:*clusterName}
			if event.Action == "die" || event.Action == "stop" || event.Action == "kill" {
				containerInfo.fillExitedContainerInfo(cli, event.ID)
			}
			go publish(&containerInfo, svc, *arn)
			log.Print(event.Action, containerInfo)
			//fmt.Printf("%+v\n", containerInfo)
		}
		time.Sleep(10 * time.Second)
		log.Println(" try reinit client ")
	}
}
func calcStatus(action string) string {
	switch action {
	case "oom":
		return "FAILURE"
	case "create":
		return "CREATED"
	case "start":
		return "RUNNING"
	case "kill":
	case "stop":
		return "STOP"
	default:
		return strings.ToUpper(action)
	}
	return strings.ToUpper(action)
}

func (info *ECSContainerInfo) fillExitedContainerInfo(cli *client.Client, id string) (error) {
	containerJSON, err := cli.ContainerInspect(context.Background(), id)
	info.ExitCode = containerJSON.State.ExitCode
	if containerJSON.State.ExitCode != 0 {
		info.Status = "FAILURE"
	} else if !containerJSON.State.Paused && !containerJSON.State.Restarting && !containerJSON.State.Running {
		info.Status = "SUCCESS"
	}
	if containerJSON.State.OOMKilled {
		info.FailureReason = "oom"
		info.Status = "FAILURE"
	}
	time, err := calcTimeInSec(containerJSON.State.StartedAt, containerJSON.State.FinishedAt)
	if err == nil && time > 0 {
		info.Time = strconv.Itoa(time)
	}
	return err
}

func calcTimeInSec(startedAt string, finishedAt string) (int, error) {
	start, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return 0, err
	}
	end, err := time.Parse(time.RFC3339, finishedAt)
	if err != nil {
		return 0, err
	}
	return int(end.Unix() - start.Unix()), nil;
}

func getTaskInfo(id string) (TaskInfo, error) {
	if _, ok := tasks[id]; !ok {
		task := new(TaskInfo)
		err := readFromJson("http://localhost:51678/v1/tasks?dockerid=" + id, task)
		if err != nil {
			return *task, err
		}
		if task.Arn == "" {
			return *task, errors.New("no ecs id for container " + id)
		}
		tasks[id] = *task
	}
	return tasks[id], nil
}

func readFromJson(url string, target interface{}) (error) {
	r, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(target)
}

func publish(message *ECSContainerInfo, svc *sns.SNS, arn string) (*sns.PublishOutput, error) {
	b, err := json.Marshal(message)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	params := &sns.PublishInput{
		Message: aws.String(string(b)), // This is the message itself (can be XML / JSON / Text - anything you want)
		TopicArn: aws.String(arn),  //Get this from the Topic in the AWS console.
	}
	return svc.Publish(params)
}