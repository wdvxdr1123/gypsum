package gypsum

import (
	"encoding/gob"
	"errors"

	log "github.com/sirupsen/logrus"
)

type ItemType string

const (
	RuleItem      ItemType = "rule"
	TriggerItem   ItemType = "trigger"
	SchedulerItem ItemType = "scheduler"
	ResourceItem  ItemType = "resource"
	GroupItem     ItemType = "group"
)

type UserRecord interface {
	ToBytes() ([]byte, error)
	GetParentID() uint64
	NewParent(selfID, parentID uint64) error
	SaveToDB(selfID uint64) error
}

func init() {
	gob.Register(Group{})
	gob.Register(Job{})
	gob.Register(Resource{})
	gob.Register(Rule{})
	gob.Register(Trigger{})
}

func RestoreFromUserRecord(itemType ItemType, itemBytes []byte) (uint64, error) {
	switch itemType {
	case RuleItem:
		rule, err := RuleFromBytes(itemBytes)
		if err != nil {
			return 0, err
		}
		cursor++
		if err := db.Put([]byte("gypsum-$meta-cursor"), U64ToBytes(cursor), nil); err != nil {
			return 0, err
		}
		rules[cursor] = rule
		if err := rule.SaveToDB(cursor); err != nil {
			return 0, err
		}
		return cursor, nil
	case TriggerItem:
		trigger, err := TriggerFromByte(itemBytes)
		if err != nil {
			return 0, err
		}
		cursor++
		if err := db.Put([]byte("gypsum-$meta-cursor"), U64ToBytes(cursor), nil); err != nil {
			return 0, err
		}
		triggers[cursor] = trigger
		if err := trigger.SaveToDB(cursor); err != nil {
			return 0, err
		}
		return cursor, nil
	case SchedulerItem:
		job, err := JobFromBytes(itemBytes)
		if err != nil {
			return 0, err
		}
		cursor++
		if err := db.Put([]byte("gypsum-$meta-cursor"), U64ToBytes(cursor), nil); err != nil {
			return 0, err
		}
		jobs[cursor] = job
		if err := job.SaveToDB(cursor); err != nil {
			return 0, err
		}
		return cursor, nil
	case ResourceItem:
		resource, err := ResourceFromBytes(itemBytes)
		if err != nil {
			return 0, err
		}
		cursor++
		if err := db.Put([]byte("gypsum-$meta-cursor"), U64ToBytes(cursor), nil); err != nil {
			return 0, err
		}
		resources[cursor] = resource
		if err := resource.SaveToDB(cursor); err != nil {
			return 0, err
		}
		return cursor, nil
	case GroupItem:
		err := errors.New("group in group are not supported yet")
		log.Warning(err)
		return 0, err
	default:
		err := errors.New("unexpected type of user_record")
		log.Warningf("unknown type: %s", itemType)
		return 0, err
	}
}
