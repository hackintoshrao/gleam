package scheduler

import (
	"sync"
	"time"

	"github.com/chrislusf/gleam/distributed/driver/scheduler/market"
	"github.com/chrislusf/gleam/distributed/plan"
	"github.com/chrislusf/gleam/flow"
	"github.com/chrislusf/gleam/pb"
	"github.com/chrislusf/gleam/util"
)

type SubmitTaskGroup struct {
	FlowContext *flow.FlowContext
	TaskGroup   *plan.TaskGroup
	Bid         float64
	WaitGroup   *sync.WaitGroup
}

type TaskGroupStatus struct {
	FlowContext *flow.FlowContext
	TaskGroup   *plan.TaskGroup
	Completed   bool
	Error       error
}

type ReleaseTaskGroupInputs struct {
	FlowContext *flow.FlowContext
	TaskGroups  []*plan.TaskGroup
	WaitGroup   *sync.WaitGroup
}

/*
resources are leased to driver, expires every X miniute unless renewed.
1. request resource
2. release resource
*/
func (s *Scheduler) EventLoop() {
	for {
		event := <-s.EventChan
		switch event := event.(type) {
		default:
		case SubmitTaskGroup:
			// fmt.Printf("processing %+v\n", event)
			taskGroup := event.TaskGroup
			go func() {
				defer event.WaitGroup.Done()
				tasks := event.TaskGroup.Tasks
				lastTask := tasks[len(tasks)-1]
				if tasks[0].Step.IsOnDriverSide {
					// these should be only one task on the driver side
					s.localExecute(event.FlowContext, lastTask, event.WaitGroup)
				} else {
					if !needsInputFromDriver(tasks[0]) {
						// wait until inputs are registed
						s.shardLocator.waitForInputDatasetShardLocations(tasks[0])
					}
					if isInputOnDisk(tasks[0]) && !isRestartableTasks(tasks) {
						// for non-restartable taskGroup, wait until on disk inputs are completed
						for _, stepGroup := range event.TaskGroup.ParentStepGroup.Parents {
							stepGroup.WaitForAllTasksToComplete()
						}
					}

					// fmt.Printf("inputs of %s is %s\n", tasks[0].Name(), s.allInputLocations(tasks[0]))

					pickedServerChan := make(chan market.Supply, 1)
					s.Market.AddDemand(market.Requirement(taskGroup), event.Bid, pickedServerChan)

					// get assigned executor location
					supply := <-pickedServerChan
					allocation := supply.Object.(*pb.Allocation)
					defer s.Market.ReturnSupply(supply)

					if needsInputFromDriver(tasks[0]) {
						// tell the driver to write to me
						for _, shard := range tasks[0].InputShards {
							// println("registering", shard.Name(), "at", allocation.Location.URL())
							s.setShardLocation(shard, pb.DataLocation{
								Name:     shard.Name(),
								Location: allocation.Location,
								OnDisk:   shard.Dataset.GetIsOnDiskIO(),
							})
						}
					}

					for _, shard := range lastTask.OutputShards {
						// println("registering", shard.Name(), "at", allocation.Location.URL(), "onDisk", shard.Dataset.GetIsOnDiskIO())
						s.setShardLocation(shard, pb.DataLocation{
							Name:     shard.Name(),
							Location: allocation.Location,
							OnDisk:   shard.Dataset.GetIsOnDiskIO(),
						})
					}

					fn := func() error {
						err := s.remoteExecuteOnLocation(event.FlowContext, taskGroup, allocation, event.WaitGroup)
						taskGroup.MarkStop(err)
						return err
					}

					if isRestartableTasks(tasks) {
						// s.ResultChan <-
						util.TimeDelayedRetry(fn, time.Minute, 3*time.Minute)
					} else {
						// s.ResultChan <- fn()
						fn()
					}
				}
			}()
		case ReleaseTaskGroupInputs:
			go func() {
				defer event.WaitGroup.Done()

				var wg sync.WaitGroup
				for _, taskGroup := range event.TaskGroups {
					tasks := taskGroup.Tasks
					for _, shard := range tasks[len(tasks)-1].OutputShards {
						location, _ := s.getShardLocation(shard)
						// println("deleting", shard.Name(), "on", location.URL())
						if location.Location == nil {
							continue
						}
						wg.Add(1)
						go func(location pb.DataLocation, shard *flow.DatasetShard) {
							defer wg.Done()
							if err := sendDeleteRequest(location.Location.URL(), &pb.DeleteDatasetShardRequest{
								Name: shard.Name(),
							}); err != nil {
								println("Purging dataset error:", err.Error())
							}
						}(location, shard)
					}
				}
				wg.Wait()

			}()
		}
	}
}

func needsInputFromDriver(task *flow.Task) bool {
	for _, shard := range task.InputShards {
		if shard.Dataset.Step.IsOnDriverSide {
			return true
		}
	}
	return false
}

func isInputOnDisk(task *flow.Task) bool {
	for _, shard := range task.InputShards {
		if shard.Dataset.Meta.OnDisk != flow.ModeOnDisk {
			return false
		}
	}
	return true
}

func isRestartableTasks(tasks []*flow.Task) bool {
	for _, task := range tasks {
		if !task.Step.Meta.IsRestartable {
			return false
		}
	}
	return true
}

func (s *Scheduler) getShardLocation(shard *flow.DatasetShard) (pb.DataLocation, bool) {
	location, found := s.shardLocator.GetShardLocation(shard.Name())
	return location, found
}

func (s *Scheduler) setShardLocation(shard *flow.DatasetShard, loc pb.DataLocation) {
	s.shardLocator.SetShardLocation(shard.Name(), loc)
}
