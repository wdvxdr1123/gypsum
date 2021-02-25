package gypsum

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"strconv"
	"strings"

	"github.com/flosch/pongo2"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"
	"github.com/syndtr/goleveldb/leveldb/util"
	lua "github.com/yuin/gopher-lua"

	"github.com/yuudi/gypsum/gypsum/helper"
)

type Job struct {
	DisplayName string  `json:"display_name"`
	Active      bool    `json:"active"`
	GroupsID    []int64 `json:"groups_id"`
	UsersID     []int64 `json:"users_id"`
	Once        bool    `json:"once"`
	CronSpec    string  `json:"cron_spec"`
	Action      string  `json:"action"`
	ParentGroup uint64  `json:"-"`
}

var (
	scheduler *cron.Cron
	jobs      map[uint64]*Job
	entries   map[uint64]cron.EntryID
)

var specParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func (j *Job) ToBytes() ([]byte, error) {
	buffer := bytes.Buffer{}
	encoder := gob.NewEncoder(&buffer)
	if err := encoder.Encode(j); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func JobFromBytes(b []byte) (*Job, error) {
	j := &Job{
		GroupsID: []int64{},
		UsersID:  []int64{},
	}
	decoder := gob.NewDecoder(bytes.NewReader(b))
	err := decoder.Decode(j)
	return j, err
}

func (j *Job) Executor() (func(), *uint64, error) {
	tmpl, err := pongo2.FromString(j.Action)
	if err != nil {
		return nil, nil, err
	}
	jobID := ^uint64(0)
	return func() {
		var luaState *lua.LState
		defer func() {
			if luaState != nil {
				luaState.Close()
			}
		}()
		msg, err := tmpl.Execute(pongo2.Context{
			"_lua": luaState,
		})
		if err != nil {
			log.Errorf("渲染模板出错：%s", err)
			return
		}
		msg = strings.TrimSpace(msg)
		if msg != "" {
			for _, friend := range j.UsersID {
				Bot.SendPrivateMessage(friend, msg)
			}
			for _, group := range j.GroupsID {
				Bot.SendGroupMessage(group, msg)
			}
			log.Infof("scheduled job executed: %s", msg)
		}
		if j.Once {
			delete(jobs, jobID)
			scheduler.Remove(entries[jobID])
			if err := db.Delete(append([]byte("gypsum-jobs-"), helper.U64ToBytes(jobID)...), nil); err != nil {
				log.Errorf("delete job from database error: %s", err)
			}
		}
	}, &jobID, nil
}

func (j *Job) Register(id uint64) error {
	if !j.Active {
		return nil
	}
	exe, jobID, err := j.Executor()
	if err != nil {
		return err
	}
	*jobID = id
	entry, err := scheduler.AddFunc(j.CronSpec, exe)
	if err != nil {
		return err
	}
	entries[id] = entry
	return nil
}

func loadJobs() {
	scheduler = cron.New()
	jobs = make(map[uint64]*Job)
	entries = make(map[uint64]cron.EntryID)
	iter := db.NewIterator(util.BytesPrefix([]byte("gypsum-jobs-")), nil)
	defer func() {
		iter.Release()
		if err := iter.Error(); err != nil {
			log.Errorf("载入数据错误：%s", err)
		}
	}()
	for iter.Next() {
		key := helper.ToUint(iter.Key()[12:])
		value := iter.Value()
		j, e := JobFromBytes(value)
		if e != nil {
			log.Errorf("无法加载任务%d：%s", key, e)
			continue
		}
		jobs[key] = j
		if e := j.Register(key); e != nil {
			log.Errorf("无法注册任务%d：%s", key, e)
			continue
		}
	}
	go scheduler.Start()
}

func (j *Job) SaveToDB(idx uint64) error {
	v, err := j.ToBytes()
	if err != nil {
		return err
	}
	return db.Put(append([]byte("gypsum-jobs-"), helper.U64ToBytes(idx)...), v, nil)
}

func (j *Job) GetParentID() uint64 {
	return j.ParentGroup
}

func (j *Job) GetDisplayName() string {
	return j.DisplayName
}

func (j *Job) NewParent(selfID, parentID uint64) error {
	j.ParentGroup = parentID
	v, err := j.ToBytes()
	if err != nil {
		return err
	}
	err = db.Put(append([]byte("gypsum-jobs-"), helper.U64ToBytes(selfID)...), v, nil)
	return err
}

func getJobs(c *gin.Context) {
	c.JSON(200, jobs)
}

func getJobByID(c *gin.Context) {
	jobIDStr := c.Param("jid")
	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such job",
		})
		return
	}
	r, ok := jobs[jobID]
	if ok {
		c.JSON(200, r)
		return
	}
	c.JSON(404, gin.H{
		"code":    1000,
		"message": "no such job",
	})
}

func createJob(c *gin.Context) {
	var job Job
	if err := c.BindJSON(&job); err != nil {
		c.JSON(400, gin.H{
			"code":    2000,
			"message": fmt.Sprintf("converting error: %s", err),
		})
		return
	}
	parentStr := c.Param("gid")
	var parentID uint64
	if len(parentStr) == 0 {
		parentID = 0
	} else {
		var err error
		parentID, err = strconv.ParseUint(parentStr, 10, 64)
		if err != nil {
			c.JSON(404, gin.H{
				"code":    1000,
				"message": "no such group",
			})
			return
		}
	}
	parentGroup, ok := groups[parentID]
	if !ok {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "group not found",
		})
		return
	}
	job.ParentGroup = parentID
	// check spec syntax
	_, err := specParser.Parse(job.CronSpec)
	if err != nil {
		c.JSON(422, gin.H{
			"code":    2010,
			"message": fmt.Sprintf("spec syntax error: %s", err),
		})
		return
	}
	if err := checkTemplate(job.Action); err != nil {
		c.JSON(422, gin.H{
			"code":    2041,
			"message": fmt.Sprintf("template error: %s", err),
		})
		return
	}
	// save
	cursor := itemCursor.Require()
	parentGroup.Items = append(parentGroup.Items, Item{
		ItemType:    SchedulerItem,
		DisplayName: job.DisplayName,
		ItemID:      cursor,
	})
	if err := parentGroup.SaveToDB(parentID); err != nil {
		log.Error(err)
		c.JSON(500, gin.H{
			"code":    3000,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	v, err := job.ToBytes()
	if err != nil {
		c.JSON(400, gin.H{
			"code":    2000,
			"message": fmt.Sprintf("converting error: %s", err),
		})
		return
	}
	// register
	if err := job.Register(cursor); err != nil {
		c.JSON(400, gin.H{
			"code":    2001,
			"message": fmt.Sprintf("job error: %s", err),
		})
		return
	}
	if err := db.Put(append([]byte("gypsum-jobs-"), helper.U64ToBytes(cursor)...), v, nil); err != nil {
		c.JSON(500, gin.H{
			"code":    3000,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	jobs[cursor] = &job
	c.JSON(201, gin.H{
		"code":    0,
		"message": "ok",
		"job_id":  cursor,
	})
	return
}

func deleteJob(c *gin.Context) {
	jobIDStr := c.Param("jid")
	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such job",
		})
		return
	}
	job, ok := jobs[jobID]
	if !ok {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such job",
		})
		return
	}
	// remove self from parent
	if err := DeleteFromParent(job.ParentGroup, jobID); err != nil {
		log.Errorf("error when delete group %d from parent group %d: %s", jobID, job.ParentGroup, err)
	}
	// remove self from database
	delete(jobs, jobID)
	if err := db.Delete(append([]byte("gypsum-jobs-"), helper.U64ToBytes(jobID)...), nil); err != nil {
		c.JSON(500, gin.H{
			"code":    3001,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	if job.Active {
		scheduler.Remove(entries[jobID])
	}
	c.JSON(200, gin.H{
		"code":    0,
		"message": "deleted",
	})
	return
}

func modifyJob(c *gin.Context) {
	jobIDStr := c.Param("jid")
	jobID, err := strconv.ParseUint(jobIDStr, 10, 64)
	if err != nil {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such job",
		})
		return
	}
	oldJob, ok := jobs[jobID]
	if !ok {
		c.JSON(404, gin.H{
			"code":    100,
			"message": "no such job",
		})
		return
	}
	var newJob Job
	if err := c.BindJSON(&newJob); err != nil {
		c.JSON(400, gin.H{
			"code":    2000,
			"message": fmt.Sprintf("converting error: %s", err),
		})
		return
	}
	// check spec syntax
	_, err = specParser.Parse(newJob.CronSpec)
	if err != nil {
		c.JSON(422, gin.H{
			"code":    2010,
			"message": fmt.Sprintf("spec syntax error: %s", err),
		})
		return
	}
	if err := checkTemplate(newJob.Action); err != nil {
		c.JSON(422, gin.H{
			"code":    2041,
			"message": fmt.Sprintf("template error: %s", err),
		})
		return
	}
	newJob.ParentGroup = oldJob.ParentGroup
	if oldJob.Active {
		scheduler.Remove(entries[jobID])
	}
	if err := newJob.Register(jobID); err != nil {
		c.JSON(400, gin.H{
			"code":    2001,
			"message": fmt.Sprintf("job error: %s", err),
		})
		return
	}
	if err := newJob.SaveToDB(jobID); err != nil {
		c.JSON(500, gin.H{
			"code":    3002,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	jobs[jobID] = &newJob
	if newJob.DisplayName != oldJob.DisplayName {
		if err = ChangeNameForParent(newJob.ParentGroup, jobID, newJob.DisplayName); err != nil {
			log.Errorf("error when change job %d from parent group %d: %s", jobID, newJob.ParentGroup, err)
		}
	}
	c.JSON(200, gin.H{
		"code":    0,
		"message": "ok",
	})
	return
}
