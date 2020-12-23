package gypsum

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log"
	"strconv"

	"github.com/flosch/pongo2"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"github.com/syndtr/goleveldb/leveldb/util"
	zero "github.com/wdvxdr1123/ZeroBot"
)

type Job struct {
	Active   bool    `json:"active"`
	GroupID  []int64 `json:"group_id"`
	UserID   []int64 `json:"user_id"`
	Once     bool    `json:"once"`
	CronSpec string  `json:"cron_spec"`
	Action   string  `json:"action"`
}

var (
	scheduler *cron.Cron
	jobs      map[uint64]Job
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
		Active:   true,
		GroupID:  []int64{},
		UserID:   []int64{},
		Once:     false,
		CronSpec: "0 0 * * *",
		Action:   "",
	}
	buffer := bytes.Buffer{}
	buffer.Write(b)
	decoder := gob.NewDecoder(&buffer)
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
		msg, err := tmpl.Execute(pongo2.Context{})
		if err != nil {
			log.Printf("渲染模板出错：%s", err)
			return
		}
		for _, friend := range j.UserID {
			zero.SendPrivateMessage(friend, msg)
		}
		for _, group := range j.GroupID {
			zero.SendGroupMessage(group, msg)
		}
		log.Println(msg)
		if j.Once {
			delete(jobs, jobID)
			scheduler.Remove(entries[jobID])
			if err := db.Delete(append([]byte("gypsum-jobs-"), ToBytes(jobID)...), nil); err != nil {
				log.Printf("delete job from database error: %s", err)
			}
		}
	}, &jobID, nil
}

func (j *Job) Register(id uint64) error {
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
	jobs = make(map[uint64]Job)
	entries = make(map[uint64]cron.EntryID)
	iter := db.NewIterator(util.BytesPrefix([]byte("gypsum-jobs-")), nil)
	defer func() {
		iter.Release()
		if err := iter.Error(); err != nil {
			log.Printf("载入数据错误：%s", err)
		}
	}()
	for iter.Next() {
		key := ToUint(iter.Key()[12:])
		value := iter.Value()
		j, e := JobFromBytes(value)
		if e != nil {
			log.Printf("无法加载任务%d：%s", key, e)
			continue
		}
		jobs[key] = *j
		if e := j.Register(key); e != nil {
			log.Printf("无法注册任务%d：%s", key, e)
			continue
		}
	}
	go scheduler.Start()
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
	} else {
		r, ok := jobs[jobID]
		if ok {
			c.JSON(200, r)
		} else {
			c.JSON(404, gin.H{
				"code":    1000,
				"message": "no such job",
			})
		}
	}
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
	// check spec syntax
	_, err := specParser.Parse(job.CronSpec)
	if err != nil {
		c.JSON(422, gin.H{
			"code":    2010,
			"message": fmt.Sprintf("spec syntax error: %s", err),
		})
		return
	}
	cursor++
	if err := db.Put([]byte("gypsum-$meta-cursor"), ToBytes(cursor), nil); err != nil {
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
	if err := job.Register(cursor); err != nil {
		c.JSON(400, gin.H{
			"code":    2001,
			"message": fmt.Sprintf("job error: %s", err),
		})
		return
	}
	if err := db.Put(append([]byte("gypsum-jobs-"), ToBytes(cursor)...), v, nil); err != nil {
		c.JSON(500, gin.H{
			"code":    3000,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	jobs[cursor] = job
	c.JSON(201, gin.H{
		"code":    0,
		"message": "ok",
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
	entry, ok := entries[jobID]
	if !ok {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such job",
		})
		return
	}
	delete(jobs, jobID)
	if err := db.Delete(append([]byte("gypsum-jobs-"), ToBytes(jobID)...), nil); err != nil {
		c.JSON(500, gin.H{
			"code":    3001,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	scheduler.Remove(entry)
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
	entry, ok := entries[jobID]
	if !ok {
		c.JSON(404, gin.H{
			"code":    100,
			"message": "no such job",
		})
		return
	}
	var job Job
	if err := c.BindJSON(&job); err != nil {
		c.JSON(400, gin.H{
			"code":    2000,
			"message": fmt.Sprintf("converting error: %s", err),
		})
		return
	}
	// check spec syntax
	_, err = specParser.Parse(job.CronSpec)
	if err != nil {
		c.JSON(422, gin.H{
			"code":    2010,
			"message": fmt.Sprintf("spec syntax error: %s", err),
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
	scheduler.Remove(entry)
	if err := job.Register(jobID); err != nil {
		c.JSON(400, gin.H{
			"code":    2001,
			"message": fmt.Sprintf("job error: %s", err),
		})
		return
	}
	if err := db.Put(append([]byte("gypsum-jobs-"), ToBytes(jobID)...), v, nil); err != nil {
		c.JSON(500, gin.H{
			"code":    3002,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	jobs[jobID] = job
	c.JSON(200, gin.H{
		"code":    0,
		"message": "ok",
	})
	return
}