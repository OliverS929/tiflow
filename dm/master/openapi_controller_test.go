// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.
//
// MVC for dm-master's openapi server
// Model(data in etcd): source of truth
// View(openapi_view): do some inner work such as validate, filter, prepare parameters/response and call controller to update model.
// Controller(openapi_controller): call model func to update data.

package master

import (
	"context"
	"fmt"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tiflow/dm/checker"
	"github.com/pingcap/tiflow/dm/config"
	"github.com/pingcap/tiflow/dm/master/scheduler"
	"github.com/pingcap/tiflow/dm/openapi"
	"github.com/pingcap/tiflow/dm/openapi/fixtures"
	"github.com/pingcap/tiflow/dm/pb"
	"github.com/pingcap/tiflow/dm/pbmock"
	"github.com/pingcap/tiflow/dm/pkg/ha"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/dm/pkg/terror"
	"github.com/stretchr/testify/suite"
)

type OpenAPIControllerSuite struct {
	suite.Suite

	testSource *openapi.Source
	testTask   *openapi.Task
}

func (s *OpenAPIControllerSuite) SetupSuite() {
	s.NoError(log.InitLogger(&log.Config{}))

	dbCfg := config.GetDBConfigForTest()
	s.testSource = &openapi.Source{
		SourceName: source1Name,
		Enable:     true,
		EnableGtid: false,
		Host:       dbCfg.Host,
		Password:   &dbCfg.Password,
		Port:       dbCfg.Port,
		User:       dbCfg.User,
	}

	task, err := fixtures.GenNoShardOpenAPITaskForTest()
	s.Nil(err)
	s.testTask = &task

	checker.CheckSyncConfigFunc = mockCheckSyncConfig
	CheckAndAdjustSourceConfigFunc = checkAndNoAdjustSourceConfigMock
	s.Nil(failpoint.Enable("github.com/pingcap/tiflow/dm/master/MockSkipAdjustTargetDB", `return(true)`))
	s.Nil(failpoint.Enable("github.com/pingcap/tiflow/dm/master/MockSkipRemoveMetaData", `return(true)`))
}

func (s *OpenAPIControllerSuite) TearDownSuite() {
	CheckAndAdjustSourceConfigFunc = checkAndAdjustSourceConfig
	checker.CheckSyncConfigFunc = checker.CheckSyncConfig
	s.Nil(failpoint.Disable("github.com/pingcap/tiflow/dm/master/MockSkipAdjustTargetDB"))
	s.Nil(failpoint.Disable("github.com/pingcap/tiflow/dm/master/MockSkipRemoveMetaData"))
}

func (s *OpenAPIControllerSuite) TestSourceController() {
	ctx, cancel := context.WithCancel(context.Background())
	server := setupTestServer(ctx, s.T())
	defer func() {
		cancel()
		server.Close()
	}()

	// reset omitted value after get
	resetValue := func(source *openapi.Source) {
		source.Purge = s.testSource.Purge
		source.Password = s.testSource.Password
		source.RelayConfig = s.testSource.RelayConfig
	}

	// create
	{
		createReq := openapi.CreateSourceRequest{
			Source: *s.testSource,
		}
		source, err := server.createSource(ctx, createReq)
		s.NoError(err)
		s.EqualValues(s.testSource, source)
	}

	// update
	{
		source := *s.testSource
		source.EnableGtid = !source.EnableGtid
		updateReq := openapi.UpdateSourceRequest{Source: source}
		sourceAfterUpdated, err := server.updateSource(ctx, source.SourceName, updateReq)
		s.NoError(err)
		s.Equal(source.EnableGtid, sourceAfterUpdated.EnableGtid)

		// update back to continue next text
		updateReq.Source = *s.testSource
		sourceAfterUpdated, err = server.updateSource(ctx, source.SourceName, updateReq)
		s.NoError(err)
		s.EqualValues(s.testSource, sourceAfterUpdated)
	}

	// get and with status
	{
		withStatus := false
		params := openapi.DMAPIGetSourceParams{WithStatus: &withStatus}
		sourceAfterGet, err := server.getSource(ctx, s.testSource.SourceName, params)
		s.NoError(err)

		resetValue(sourceAfterGet)
		s.EqualValues(s.testSource, sourceAfterGet)
		s.Nil(sourceAfterGet.StatusList)

		// with status
		withStatus = true
		params.WithStatus = &withStatus
		sourceAfterGet, err = server.getSource(ctx, s.testSource.SourceName, params)
		s.NoError(err)
		s.NotNil(sourceAfterGet.StatusList)
		statusList := *sourceAfterGet.StatusList
		s.Len(statusList, 1)
		s.NotNil(statusList[0].ErrorMsg) // no worker, will get an error msg
		s.Contains(*statusList[0].ErrorMsg, "code=38029")
	}

	// get status
	{
		statusList, err := server.getSourceStatus(ctx, s.testSource.SourceName)
		s.NoError(err)
		s.Len(statusList, 1)
		s.NotNil(statusList[0].ErrorMsg) // no worker, will get an error msg
		s.Contains(*statusList[0].ErrorMsg, "code=38029")
	}

	// list and with status
	{
		withStatus := false
		params := openapi.DMAPIGetSourceListParams{WithStatus: &withStatus}
		sourceList, err := server.listSource(ctx, params)
		s.NoError(err)
		s.Len(sourceList, 1)
		resetValue(&sourceList[0])
		s.EqualValues(s.testSource, &sourceList[0])

		withStatus = true
		sourceList, err = server.listSource(ctx, params)
		s.NoError(err)
		s.Len(sourceList, 1)
		s.NotNil(sourceList[0].StatusList)
		statusList := *sourceList[0].StatusList
		s.Contains(*statusList[0].ErrorMsg, "code=38029") // no worker, will get an error msg
	}

	// test with fake worker enable/disable/transfer/enable/disable/purge-relay
	{
		// enable on a free worker
		s.True(terror.ErrWorkerNoStart.Equal(server.enableSource(ctx, s.testSource.SourceName))) // no free worker now

		worker1Name := "worker1"
		worker1Addr := "172.16.10.72:8262"
		s.Nil(server.scheduler.AddWorker(worker1Name, worker1Addr))
		worker1 := server.scheduler.GetWorkerByName(worker1Name)
		worker1.ToFree()
		s.NoError(server.transferSource(ctx, s.testSource.SourceName, worker1Name))
		s.Equal(worker1.Stage(), scheduler.WorkerBound)

		// disable no task now, so no error
		s.NoError(server.disableSource(ctx, s.testSource.SourceName))

		// add worker 2 and transfer
		worker2Name := "worker2"
		worker2Addr := "172.16.10.72:8263"
		s.Nil(server.scheduler.AddWorker(worker2Name, worker2Addr))
		worker2 := server.scheduler.GetWorkerByName(worker2Name)
		worker2.ToFree()
		s.NoError(server.transferSource(ctx, s.testSource.SourceName, worker2Name))
		s.Equal(worker1.Stage(), scheduler.WorkerFree)
		s.Equal(worker2.Stage(), scheduler.WorkerBound)

		// enable relay with binlog name will trigger a update source
		relayBinLogName := "mysql-bin.000001"
		enableRelayReq := openapi.EnableRelayRequest{RelayBinlogName: &relayBinLogName}
		s.NoError(server.enableRelay(ctx, s.testSource.SourceName, enableRelayReq))

		updatedSource, err := server.getSource(ctx, s.testSource.SourceName, openapi.DMAPIGetSourceParams{})
		s.NoError(err)
		s.Equal(*updatedSource.RelayConfig.RelayBinlogName, relayBinLogName)

		// disable relay
		disableRelayReq := openapi.DisableRelayRequest{}
		err = server.disableRelay(ctx, s.testSource.SourceName, disableRelayReq)
		s.NoError(err)
		updatedSource, err = server.getSource(ctx, s.testSource.SourceName, openapi.DMAPIGetSourceParams{})
		s.NoError(err)
		s.False(*updatedSource.RelayConfig.EnableRelay)

		// purge
		purgeRelayReq := openapi.PurgeRelayRequest{}
		err = server.purgeRelay(ctx, s.testSource.SourceName, purgeRelayReq)
		s.Error(err)
		s.Regexp(".*please `enable-relay` first", err.Error())
	}

	// delete
	{
		s.Nil(server.deleteSource(ctx, s.testSource.SourceName, false))
		sourceList, err := server.listSource(ctx, openapi.DMAPIGetSourceListParams{})
		s.Nil(err)
		s.Len(sourceList, 0)
	}

	// create and update no password source
	{
		// no password will use "" as password
		source := *s.testSource
		source.Password = nil
		createReq := openapi.CreateSourceRequest{Source: source}
		resp, err := server.createSource(ctx, createReq)
		s.NoError(err)
		s.EqualValues(source, *resp)
		config := server.scheduler.GetSourceCfgByID(source.SourceName)
		s.NotNil(config)
		s.Equal("", config.From.Password)

		// update to have password
		updateReq := openapi.UpdateSourceRequest{Source: *s.testSource}
		sourceAfterUpdated, err := server.updateSource(ctx, source.SourceName, updateReq)
		s.NoError(err)
		s.EqualValues(s.testSource, sourceAfterUpdated)

		// update without password will use old password
		source = *s.testSource
		source.Password = nil
		updateReq = openapi.UpdateSourceRequest{Source: source}
		sourceAfterUpdated, err = server.updateSource(ctx, source.SourceName, updateReq)
		s.NoError(err)
		s.Equal(source, *sourceAfterUpdated)
		// password is old
		config = server.scheduler.GetSourceCfgByID(source.SourceName)
		s.NotNil(config)
		s.Equal(*s.testSource.Password, config.From.Password)

		s.Nil(server.deleteSource(ctx, s.testSource.SourceName, false))
	}
}

func (s *OpenAPIControllerSuite) TestTaskController() {
	ctx, cancel := context.WithCancel(context.Background())
	server := setupTestServer(ctx, s.T())
	defer func() {
		cancel()
		server.Close()
	}()

	// create source for task
	{
		// add a mock worker
		worker1Name := "worker1"
		worker1Addr := "172.16.10.72:8262"
		s.Nil(server.scheduler.AddWorker(worker1Name, worker1Addr))
		worker1 := server.scheduler.GetWorkerByName(worker1Name)
		worker1.ToFree()

		_, err := server.createSource(ctx, openapi.CreateSourceRequest{Source: *s.testSource, WorkerName: &worker1Name})
		s.Nil(err)
		s.Equal(worker1.Stage(), scheduler.WorkerBound)
		sourceList, err := server.listSource(ctx, openapi.DMAPIGetSourceListParams{})
		s.Nil(err)
		s.Len(sourceList, 1)
	}

	// create
	{
		createTaskReq := openapi.CreateTaskRequest{Task: *s.testTask}
		res, err := server.createTask(ctx, createTaskReq)
		s.Nil(err)
		s.EqualValues(*s.testTask, res.Task)
	}

	// update
	{
		task := *s.testTask
		batch := 1000
		task.SourceConfig.IncrMigrateConf.ReplBatch = &batch
		updateReq := openapi.UpdateTaskRequest{Task: task}
		s.NoError(failpoint.Enable("github.com/pingcap/tiflow/dm/master/scheduler/operateCheckSubtasksCanUpdate", `return("success")`))
		res, err := server.updateTask(ctx, updateReq)
		s.NoError(err)
		s.EqualValues(task.SourceConfig.IncrMigrateConf, res.Task.SourceConfig.IncrMigrateConf)

		// update back to continue next text
		updateReq.Task = *s.testTask
		res, err = server.updateTask(ctx, updateReq)
		s.NoError(err)
		s.EqualValues(*s.testTask, res.Task)
		s.NoError(failpoint.Disable("github.com/pingcap/tiflow/dm/master/scheduler/operateCheckSubtasksCanUpdate"))
	}

	// get and with status
	{
		withStatus := false
		params := openapi.DMAPIGetTaskParams{WithStatus: &withStatus}
		taskAfterGet, err := server.getTask(ctx, s.testTask.Name, params)
		s.NoError(err)
		s.EqualValues(s.testTask, taskAfterGet)
		s.Nil(taskAfterGet.StatusList)

		// with status
		withStatus = true
		params.WithStatus = &withStatus
		taskAfterGet, err = server.getTask(ctx, s.testTask.Name, params)
		s.NoError(err)
		s.NotNil(taskAfterGet.StatusList)
		statusList := *taskAfterGet.StatusList
		s.Len(statusList, 1)
		s.NotNil(statusList[0].ErrorMsg) // no true worker, will get an error msg
		s.Contains(*statusList[0].ErrorMsg, "code=38008")
	}

	// get status
	{
		statusList, err := server.getTaskStatus(ctx, s.testTask.Name)
		s.NoError(err)
		s.Len(statusList, 1)
		s.NotNil(statusList[0].ErrorMsg) // no worker, will get an error msg
		s.Contains(*statusList[0].ErrorMsg, "code=38008")
	}

	// list and with status
	{
		withStatus := false
		params := openapi.DMAPIGetTaskListParams{WithStatus: &withStatus}
		taskList, err := server.listTask(ctx, params)
		s.NoError(err)
		s.Len(taskList, 1)
		s.EqualValues(s.testTask, &taskList[0])

		withStatus = true
		taskList, err = server.listTask(ctx, params)
		s.NoError(err)
		s.Len(taskList, 1)
		s.NotNil(taskList[0].StatusList)
		statusList := *taskList[0].StatusList
		s.Contains(*statusList[0].ErrorMsg, "code=38008")
	}

	// start and stop
	{
		// can't start when source not enabled
		req := openapi.StartTaskRequest{}
		s.Nil(server.disableSource(ctx, s.testSource.SourceName))
		s.True(terror.ErrMasterStartTask.Equal(server.startTask(ctx, s.testTask.Name, req)))
		// start success
		s.Nil(server.enableSource(ctx, s.testSource.SourceName))
		s.Nil(server.startTask(ctx, s.testTask.Name, req))
		s.Equal(server.scheduler.GetExpectSubTaskStage(s.testTask.Name, s.testSource.SourceName).Expect, pb.Stage_Running)

		// stop success
		s.Nil(server.stopTask(ctx, s.testTask.Name, openapi.StopTaskRequest{}))
		s.Equal(server.scheduler.GetExpectSubTaskStage(s.testTask.Name, s.testSource.SourceName).Expect, pb.Stage_Stopped)

		// start with cli args
		startTime := "2022-05-05 12:12:12"
		safeModeTimeDuration := "10s"
		req = openapi.StartTaskRequest{
			StartTime:            &startTime,
			SafeModeTimeDuration: &safeModeTimeDuration,
		}
		s.Nil(server.startTask(ctx, s.testTask.Name, req))
		taskCliConf, err := ha.GetTaskCliArgs(server.etcdClient, s.testTask.Name, s.testSource.SourceName)
		s.Nil(err)
		s.NotNil(taskCliConf)
		s.Equal(startTime, taskCliConf.StartTime)
		s.Equal(safeModeTimeDuration, taskCliConf.SafeModeDuration)

		// stop success
		s.Nil(server.stopTask(ctx, s.testTask.Name, openapi.StopTaskRequest{}))
		s.Equal(server.scheduler.GetExpectSubTaskStage(s.testTask.Name, s.testSource.SourceName).Expect, pb.Stage_Stopped)
	}

	// delete
	{
		s.Nil(server.deleteTask(ctx, s.testTask.Name, true)) // delete with fore
		taskList, err := server.listTask(ctx, openapi.DMAPIGetTaskListParams{})
		s.Nil(err)
		s.Len(taskList, 0)
	}

	// convert between task and taskConfig
	{
		// from task to taskConfig
		req := openapi.ConverterTaskRequest{Task: s.testTask}
		task, taskCfg, err := server.convertTaskConfig(ctx, req)
		s.NoError(err)
		s.NotNil(task)
		s.NotNil(taskCfg)
		s.EqualValues(s.testTask, task)

		// from taskCfg to task
		req.Task = nil
		taskCfgStr := taskCfg.String()
		req.TaskConfigFile = &taskCfgStr
		task2, taskCfg2, err := server.convertTaskConfig(ctx, req)
		s.NoError(err)
		s.NotNil(task2)
		s.NotNil(taskCfg2)
		// the `security` field not support yaml format yet, it cannot marshal/unmarshal from taskCfg to string.
		if task.SourceConfig.FullMigrateConf.Security != nil {
			task2.SourceConfig.FullMigrateConf.Security = task.SourceConfig.FullMigrateConf.Security
		}
		s.EqualValues(task2, task)
		s.Equal(taskCfg2.String(), taskCfg.String())

		// incremental task without source meta
		taskTest := *s.testTask
		taskTest.TaskMode = config.ModeIncrement
		req = openapi.ConverterTaskRequest{Task: &taskTest}
		task3, taskCfg3, err := server.convertTaskConfig(ctx, req)
		s.NoError(err)
		s.NotNil(task3)
		s.NotNil(taskCfg3)
		s.EqualValues(&taskTest, task3)

		req.Task = nil
		taskCfgStr = taskCfg3.String()
		req.TaskConfigFile = &taskCfgStr
		task4, taskCfg4, err := server.convertTaskConfig(ctx, req)
		s.NoError(err)
		s.NotNil(task4)
		s.NotNil(taskCfg4)
		// the `security` field not support yaml format yet, it cannot marshal/unmarshal from taskCfg to string.
		if task3.SourceConfig.FullMigrateConf.Security != nil {
			task4.SourceConfig.FullMigrateConf.Security = task3.SourceConfig.FullMigrateConf.Security
		}
		s.EqualValues(task4, task3)
		s.Equal(taskCfg4.String(), taskCfg3.String())
	}
}

func (s *OpenAPIControllerSuite) TestTaskControllerWithInvalidTask() {
	ctx, cancel := context.WithCancel(context.Background())
	server := setupTestServer(ctx, s.T())
	defer func() {
		cancel()
		server.Close()
	}()

	// create an invalid task
	task, err := fixtures.GenNoShardErrNameOpenAPITaskForTest()
	s.NoError(err)

	// create source for task
	{
		// add a mock worker
		worker1Name := "worker1"
		worker1Addr := "172.16.10.72:8262"
		s.NoError(server.scheduler.AddWorker(worker1Name, worker1Addr))
		worker1 := server.scheduler.GetWorkerByName(worker1Name)
		worker1.ToFree()

		// create the corresponding mock worker
		ctrl := gomock.NewController(s.T())
		defer ctrl.Finish()
		mockWorkerClient := pbmock.NewMockWorkerClient(ctrl)
		queryResp := &pb.QueryStatusResponse{
			Result: true,
			Msg:    "",
			SourceStatus: &pb.SourceStatus{
				Source: s.testSource.SourceName,
				Worker: worker1Name,
			},
			SubTaskStatus: [](*pb.SubTaskStatus){&pb.SubTaskStatus{
				Result: &pb.ProcessResult{
					Errors: [](*pb.ProcessError){&pb.ProcessError{
						ErrCode:  10006,
						ErrClass: "database",
						ErrScope: "downstream",
						ErrLevel: "high",
						Message:  fmt.Sprintf("fail to initialize unit Load of %s : execute statement failed: CREATE TABLE IF NOT EXISTS `dm_meta`.`%s` (\n\t\ttask_name varchar(255) NOT NULL,\n\t\tsource_name varchar(255) NOT NULL,\n\t\tstatus varchar(10) NOT NULL DEFAULT 'init' COMMENT 'init,running,finished',\n\t\tPRIMARY KEY (task_name, source_name)\n\t);\n", task.Name, task.Name),
						RawCause: fmt.Sprintf("Error 1059: Identifier name '%s' is too long.", task.Name),
					}},
				},
			}},
		}
		mockWorkerClient.EXPECT().QueryStatus(
			gomock.Any(),
			gomock.Any(),
		).Return(queryResp, nil).MaxTimes(maxRetryNum)
		server.scheduler.SetWorkerClientForTest(worker1Name, newMockRPCClient(mockWorkerClient))

		_, err := server.createSource(ctx, openapi.CreateSourceRequest{Source: *s.testSource, WorkerName: &worker1Name})
		s.NoError(err)
		s.Equal(worker1.Stage(), scheduler.WorkerBound)
		sourceList, err := server.listSource(ctx, openapi.DMAPIGetSourceListParams{})
		s.NoError(err)
		s.Len(sourceList, 1)
	}

	// create
	{
		createTaskReq := openapi.CreateTaskRequest{Task: task}
		res, err := server.createTask(ctx, createTaskReq)
		s.NoError(err)
		s.EqualValues(task, res.Task)
	}

	// start and stop
	{
		// start success
		req := openapi.StartTaskRequest{}
		s.NoError(server.enableSource(ctx, s.testSource.SourceName))
		s.NoError(server.startTask(ctx, task.Name, req))
		s.Equal(server.scheduler.GetExpectSubTaskStage(task.Name, s.testSource.SourceName).Expect, pb.Stage_Running)

		// get status
		{
			statusList, err := server.getTaskStatus(ctx, task.Name)
			s.NoError(err)
			s.Len(statusList, 1)
			s.NotNil(statusList[0].ErrorMsg)
			s.Contains(*statusList[0].ErrorMsg, "Error 1059: ") // database error, will return an error message
		}

		// stop success
		s.NoError(server.stopTask(ctx, task.Name, openapi.StopTaskRequest{}))
		s.Equal(server.scheduler.GetExpectSubTaskStage(task.Name, s.testSource.SourceName).Expect, pb.Stage_Stopped)
	}

	// delete
	{
		s.NoError(server.deleteTask(ctx, task.Name, true)) // delete with fore
		taskList, err := server.listTask(ctx, openapi.DMAPIGetTaskListParams{})
		s.NoError(err)
		s.Len(taskList, 0)
	}
}

func TestOpenAPIControllerSuite(t *testing.T) {
	suite.Run(t, new(OpenAPIControllerSuite))
}
