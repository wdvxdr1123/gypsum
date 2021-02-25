package gypsum

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/flosch/pongo2"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/syndtr/goleveldb/leveldb/util"
	zero "github.com/wdvxdr1123/ZeroBot"
	lua "github.com/yuin/gopher-lua"

	"github.com/yuudi/gypsum/gypsum/helper"
)

type RuleType int
type MessageType uint32
type UserRole uint32

const (
	FullMatch RuleType = iota
	Keyword
	Prefix
	Suffix
	Command
	Regex
)

const (
	FriendMessage MessageType = 1 << iota
	GroupTmpMessage
	OtherTmpMessage
	OfficialMessage
	GroupNormalMessage
	GroupAnonymousMessage
	GroupNoticeMessage
	DiscussMessage

	NoMessage      MessageType = 0
	AllMessage     MessageType = 0xffff
	PrivateMessage             = FriendMessage | GroupTmpMessage | OtherTmpMessage
	GroupMessage               = GroupNormalMessage | GroupAnonymousMessage | GroupNoticeMessage
)

const (
	GroupMemberUserRole UserRole = 1 << iota
	GroupAdminUserRole
	GroupOwnerUserRole
	BotAdminUserRole

	AllGroupUserRole = GroupMemberUserRole | GroupAdminUserRole | GroupOwnerUserRole
)

type Rule struct {
	DisplayName string             `json:"display_name"`
	Active      bool               `json:"active"`
	MessageType MessageType        `json:"message_type"`
	GroupsID    []int64            `json:"groups_id"`
	UsersID     []int64            `json:"users_id"`
	Role        UserRole           `json:"role"`
	RateLimit   *LimiterDescriptor `json:"rate_limit"`
	MatcherType RuleType           `json:"matcher_type"`
	Patterns    []string           `json:"patterns"`
	OnlyAtMe    bool               `json:"only_at_me"`
	Response    string             `json:"response"`
	Priority    int                `json:"priority"`
	Block       bool               `json:"block"`
	ParentGroup uint64             `json:"-"`
}

var (
	rules       map[uint64]*Rule
	zeroMatcher map[uint64]*zero.Matcher
)

func (r *Rule) ToBytes() ([]byte, error) {
	buffer := bytes.Buffer{}
	encoder := gob.NewEncoder(&buffer)
	if err := encoder.Encode(r); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func RuleFromBytes(b []byte) (*Rule, error) {
	r := &Rule{
		GroupsID: []int64{},
		UsersID:  []int64{},
		Patterns: []string{},
	}
	decoder := gob.NewDecoder(bytes.NewReader(b))
	err := decoder.Decode(r)
	return r, err
}

func (u UserRole) ToGroupRoleList() (roleList []string) {
	if u&GroupMemberUserRole != 0 {
		roleList = append(roleList, "member")
	}
	if u&GroupAdminUserRole != 0 {
		roleList = append(roleList, "admin")
	}
	if u&GroupOwnerUserRole != 0 {
		roleList = append(roleList, "owner")
	}
	return
}

func (acceptType MessageType) ToRule() zero.Rule {
	return func(ctx *zero.Ctx) bool {
		var msgType MessageType
		switch ctx.Event.MessageType {
		case "group":
			msgType = GroupMessage
		case "private":
			msgType = PrivateMessage
		case "discuss":
			msgType = DiscussMessage
		case "official":
			msgType = OfficialMessage
		default:
			log.Warnf("未知的消息类型：%s", ctx.Event.MessageType)
			return false
		}
		return (msgType & acceptType) != NoMessage
	}
}

func (u UserRole) ToRule() zero.Rule {
	if u == 0 {
		return RuleAlwaysTrue
	}
	var filterGroupRole, filterBotAdmin bool
	var groupRoleList []string
	if (u & AllGroupUserRole) == AllGroupUserRole {
		// 三个角色都指定了，就不过滤了
		filterGroupRole = false
	} else {
		groupRoleList = u.ToGroupRoleList()
		filterGroupRole = true
	}
	filterBotAdmin = u&BotAdminUserRole != 0
	return func(ctx *zero.Ctx) bool {
		if filterGroupRole {
			if ctx.Event.Sender != nil {
				for _, role := range groupRoleList {
					if ctx.Event.Sender.Role == role {
						return true
					}
				}
			}
		}
		if filterBotAdmin {
			for _, id := range botAdmins {
				if ctx.Event.UserID == id {
					return true
				}
			}
		}
		return false
	}
}

func groupsRule(groupsID []int64) zero.Rule {
	if len(groupsID) == 0 {
		return RuleAlwaysTrue
	}
	return func(ctx *zero.Ctx) bool {
		for _, i := range groupsID {
			if i == ctx.Event.GroupID {
				return true
			}
		}
		return false
	}
}

func usersRule(usersID []int64) zero.Rule {
	if len(usersID) == 0 {
		return RuleAlwaysTrue
	}
	return func(ctx *zero.Ctx) bool {
		for _, i := range usersID {
			if i == ctx.Event.UserID {
				return true
			}
		}
		return false
	}
}

func (r *Rule) Register(id uint64) error {
	if !r.Active {
		return nil
	}
	tmpl, err := pongo2.FromString(r.Response)
	if err != nil {
		log.Errorf("模板预处理出错：%s", err)
		return err
	}
	msgRule := make([]zero.Rule, 4)
	msgRule = append(msgRule, r.MessageType.ToRule())
	if len(r.GroupsID) != 0 {
		msgRule = append(msgRule, groupsRule(r.GroupsID))
	}
	if len(r.UsersID) != 0 {
		msgRule = append(msgRule, usersRule(r.UsersID))
	}
	if r.OnlyAtMe {
		msgRule = append(msgRule, zero.OnlyToMe)
	}
	if r.Role != 0 {
		msgRule = append(msgRule, r.Role.ToRule())
	}
	if r.RateLimit != nil {
		r.RateLimit.SetDBKey(helper.U64ToBytes(id))
		msgRule = append(msgRule, r.RateLimit.ToRule())
	}
	switch r.MatcherType {
	case FullMatch:
		msgRule = append(msgRule, zero.FullMatchRule(r.Patterns...))
	case Keyword:
		msgRule = append(msgRule, zero.KeywordRule(r.Patterns...))
	case Prefix:
		msgRule = append(msgRule, zero.PrefixRule(r.Patterns...))
	case Suffix:
		msgRule = append(msgRule, zero.SuffixRule(r.Patterns...))
	case Command:
		msgRule = append(msgRule, zero.CommandRule(r.Patterns...))
	case Regex:
		if len(r.Patterns) == 0 {
			return errors.New("regex rule without pattern")
		} else {
			msgRule = append(msgRule, zero.RegexRule(r.Patterns[0]))
		}
	default:
		log.Errorf("Unknown type %#v", r.MatcherType)
		return errors.New(fmt.Sprintf("Unknown type %#v", r.MatcherType))
	}
	zeroMatcher[id] = zero.OnMessage(msgRule...).SetPriority(r.Priority).SetBlock(r.Block).Handle(templateRuleHandler(*tmpl, nil, log.Error))
	return nil
}

func templateRuleHandler(tmpl pongo2.Template, send func(msg interface{}) int64, errLogger func(...interface{})) zero.Handler {
	return func(ctx *zero.Ctx) {
		var luaState *lua.LState
		defer func() {
			if luaState != nil {
				luaState.Close()
			}
		}()
		reply, err := tmpl.Execute(buildExecutionContext(ctx, luaState))
		if err != nil {
			errLogger("渲染模板出错：" + err.Error())
			return
		}
		reply = strings.TrimSpace(reply)
		if reply != "" {
			if send != nil {
				send(reply)
			} else {
				ctx.Send(reply)
			}
		}
		return
	}
}

func loadRules() {
	rules = make(map[uint64]*Rule)
	zeroMatcher = make(map[uint64]*zero.Matcher)
	iter := db.NewIterator(util.BytesPrefix([]byte("gypsum-rules-")), nil)
	defer func() {
		iter.Release()
		if err := iter.Error(); err != nil {
			log.Errorf("载入数据错误：%s", err)
		}
	}()
	for iter.Next() {
		key := helper.ToUint(iter.Key()[13:])
		value := iter.Value()
		r, e := RuleFromBytes(value)
		if e != nil {
			log.Errorf("无法加载规则%d：%s", key, e)
			continue
		}
		rules[key] = r
		if e := r.Register(key); e != nil {
			log.Errorf("无法注册规则%d：%s", key, e)
			continue
		}
	}
}

func (r *Rule) SaveToDB(idx uint64) error {
	v, err := r.ToBytes()
	if err != nil {
		return err
	}
	return db.Put(append([]byte("gypsum-rules-"), helper.U64ToBytes(idx)...), v, nil)
}

func checkRegex(pattern string) error {
	_, err := regexp.Compile(pattern)
	return err
}

func checkTemplate(template string) error {
	_, err := pongo2.FromString(template)
	return err
}

func (r *Rule) GetParentID() uint64 {
	return r.ParentGroup
}

func (r *Rule) GetDisplayName() string {
	return r.DisplayName
}

func (r *Rule) NewParent(selfID, parentID uint64) error {
	v, err := r.ToBytes()
	if err != nil {
		return err
	}
	r.ParentGroup = parentID
	err = db.Put(append([]byte("gypsum-rules-"), helper.U64ToBytes(selfID)...), v, nil)
	return err
}

func getRules(c *gin.Context) {
	c.JSON(200, rules)
}

func getRuleByID(c *gin.Context) {
	ruleIDStr := c.Param("rid")
	ruleID, err := strconv.ParseUint(ruleIDStr, 10, 64)
	if err != nil {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such rule",
		})
	} else {
		r, ok := rules[ruleID]
		if ok {
			c.JSON(200, r)
		} else {
			c.JSON(404, gin.H{
				"code":    1000,
				"message": "no such rule",
			})
		}
	}
}

func createRule(c *gin.Context) {
	var rule Rule
	if err := c.BindJSON(&rule); err != nil {
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
	rule.ParentGroup = parentID
	// syntax check
	if rule.MatcherType == Regex {
		if len(rule.Patterns) != 1 {
			c.JSON(422, gin.H{
				"code":    2001,
				"message": fmt.Sprintf("regex mather can only accept one pattern"),
			})
			return
		}
		if err := checkRegex(rule.Patterns[0]); err != nil {
			c.JSON(422, gin.H{
				"code":    2002,
				"message": fmt.Sprintf("cannot compile regex pattern: %s", err),
			})
			return
		}
	}
	if err := checkTemplate(rule.Response); err != nil {
		c.JSON(422, gin.H{
			"code":    2041,
			"message": fmt.Sprintf("template error: %s", err),
		})
		return
	}
	// save
	cursor := itemCursor.Require()
	parentGroup.Items = append(parentGroup.Items, Item{
		ItemType:    RuleItem,
		DisplayName: rule.DisplayName,
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
	v, err := rule.ToBytes()
	if err != nil {
		c.JSON(400, gin.H{
			"code":    2000,
			"message": fmt.Sprintf("converting error: %s", err),
		})
		return
	}
	if err := rule.Register(cursor); err != nil {
		c.JSON(400, gin.H{
			"code":    2001,
			"message": fmt.Sprintf("rule error: %s", err),
		})
		return
	}
	if err := db.Put(append([]byte("gypsum-rules-"), helper.U64ToBytes(cursor)...), v, nil); err != nil {
		c.JSON(500, gin.H{
			"code":    3000,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	rules[cursor] = &rule
	c.JSON(201, gin.H{
		"code":    0,
		"message": "ok",
		"rule_id": cursor,
	})
	return
}

func deleteRule(c *gin.Context) {
	ruleIDStr := c.Param("rid")
	ruleID, err := strconv.ParseUint(ruleIDStr, 10, 64)
	if err != nil {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such rule",
		})
		return
	}
	oldRule, ok := rules[ruleID]
	if !ok {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such rule",
		})
		return
	}
	// remove self from parent
	if err := DeleteFromParent(oldRule.ParentGroup, ruleID); err != nil {
		log.Errorf("error when delete group %d from parent group %d: %s", ruleID, oldRule.ParentGroup, err)
	}
	// remove self from database
	delete(rules, ruleID)
	if err := db.Delete(append([]byte("gypsum-rules-"), helper.U64ToBytes(ruleID)...), nil); err != nil {
		c.JSON(500, gin.H{
			"code":    3001,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	if oldRule.Active {
		zeroMatcher[ruleID].Delete()
	}
	c.JSON(200, gin.H{
		"code":    0,
		"message": "deleted",
	})
	return
}

func modifyRule(c *gin.Context) {
	ruleIDStr := c.Param("rid")
	ruleID, err := strconv.ParseUint(ruleIDStr, 10, 64)
	if err != nil {
		c.JSON(404, gin.H{
			"code":    1000,
			"message": "no such rule",
		})
		return
	}
	oldRule, ok := rules[ruleID]
	if !ok {
		c.JSON(404, gin.H{
			"code":    100,
			"message": "no such rule",
		})
		return
	}
	var newRule Rule
	if err := c.BindJSON(&newRule); err != nil {
		c.JSON(400, gin.H{
			"code":    2000,
			"message": fmt.Sprintf("converting error: %s", err),
		})
		return
	}
	// check new rule syntax
	if newRule.MatcherType == Regex {
		if len(newRule.Patterns) != 1 {
			c.JSON(422, gin.H{
				"code":    2001,
				"message": fmt.Sprintf("regex mather can only accept one pattern"),
			})
			return
		}
		if err := checkRegex(newRule.Patterns[0]); err != nil {
			c.JSON(422, gin.H{
				"code":    2002,
				"message": fmt.Sprintf("cannot compile regex pattern: %s", err),
			})
			return
		}
	}
	if err := checkTemplate(newRule.Response); err != nil {
		c.JSON(422, gin.H{
			"code":    2041,
			"message": fmt.Sprintf("template error: %s", err),
		})
		return
	}
	newRule.ParentGroup = oldRule.ParentGroup
	if oldRule.Active {
		oldMatcher, ok := zeroMatcher[ruleID]
		if !ok {
			c.JSON(500, gin.H{
				"code":    7012,
				"message": "error when delete old rule: matcher not found",
			})
			return
		}
		oldMatcher.Delete()
	}
	if err := newRule.Register(ruleID); err != nil {
		c.JSON(400, gin.H{
			"code":    2001,
			"message": fmt.Sprintf("rule error: %s", err),
		})
		return
	}
	if err := newRule.SaveToDB(ruleID); err != nil {
		c.JSON(500, gin.H{
			"code":    3002,
			"message": fmt.Sprintf("Server got itself into trouble: %s", err),
		})
		return
	}
	rules[ruleID] = &newRule
	if newRule.DisplayName != oldRule.DisplayName {
		if err = ChangeNameForParent(newRule.ParentGroup, ruleID, newRule.DisplayName); err != nil {
			log.Errorf("error when change rule %d from parent group %d: %s", ruleID, newRule.ParentGroup, err)
		}
	}
	c.JSON(200, gin.H{
		"code":    0,
		"message": "ok",
	})
	return
}
