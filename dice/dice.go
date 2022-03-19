package dice

import (
	"errors"
	wr "github.com/mroth/weightedrand"
	"go.etcd.io/bbolt"
	"go.uber.org/zap"
	"math/rand"
	"os"
	"path/filepath"
	"sealdice-core/dice/logger"
	"sealdice-core/dice/model"
	"strings"
	"time"
)

var APPNAME = "SealDice"
var VERSION = "0.99.4内测版 v20220318"

type CmdExecuteResult struct {
	Matched bool // 是否是指令
	Solved  bool // 是否走到结尾
}

type CmdItemInfo struct {
	Name  string
	Solve func(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult
	Brief string
	Help  string
}

type CmdMapCls map[string]*CmdItemInfo

type ExtInfo struct {
	Name    string `yaml:"name"` // 名字
	Version string `yaml:"-"`    // 版本
	// 作者
	// 更新时间
	AutoActive      bool      `yaml:"-"` // 是否自动开启
	CmdMap          CmdMapCls `yaml:"-"` // 指令集合
	Brief           string    `yaml:"-"`
	ActiveOnPrivate bool      `yaml:"-"`

	Author       string   `yaml:"-"`
	ConflictWith []string `yaml:"-"`
	//activeInSession bool; // 在当前会话中开启

	OnCommandReceived func(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs)                             `yaml:"-"`
	OnMessageReceived func(ctx *MsgContext, msg *Message)                                               `yaml:"-"`
	OnMessageSend     func(ctx *MsgContext, messageType string, userId int64, text string, flag string) `yaml:"-"`
	GetDescText       func(i *ExtInfo) string                                                           `yaml:"-"`
	IsLoaded          bool                                                                              `yaml:"-"`
	OnLoad            func()                                                                            `yaml:"-"`
}

type DiceConfig struct {
	Name       string `yaml:"name"`       // 名称，默认为default
	DataDir    string `yaml:"dataDir"`    // 数据路径，为./data/{name}，例如data/default
	IsLogPrint bool   `yaml:"isLogPrint"` // 是否在控制台打印log
}

type Dice struct {
	ImSession               *IMSession             `yaml:"imSession"`
	CmdMap                  CmdMapCls              `yaml:"-"`
	ExtList                 []*ExtInfo             `yaml:"-"`
	RollParser              *DiceRollParser        `yaml:"-"`
	CommandCompatibleMode   bool                   `yaml:"commandCompatibleMode"`
	LastSavedTime           *time.Time             `yaml:"lastSavedTime"`
	TextMap                 map[string]*wr.Chooser `yaml:"-"`
	BaseConfig              DiceConfig             `yaml:"-"`
	DB                      *bbolt.DB              `yaml:"-"`                       // 数据库对象
	Logger                  *zap.SugaredLogger     `yaml:"logger"`                  // 日志
	LogWriter               *logger.WriterX        `yaml:"-"`                       // 用于api的log对象
	DeckList                []*DeckInfo            `yaml:"deckList"`                // 牌堆信息
	CommandPrefix           []string               `yaml:"commandPrefix"`           // 指令前导
	DiceMasters             []string               `yaml:"diceMasters"`             // 骰主设置，需要格式: 平台:帐号
	OnlyLogCommandInGroup   bool                   `yaml:"onlyLogCommandInGroup"`   // 日志中仅记录命令
	OnlyLogCommandInPrivate bool                   `yaml:"onlyLogCommandInPrivate"` // 日志中仅记录命令

	//ConfigVersion         int                    `yaml:"configVersion"`
	InPackGoCqHttpExists       bool                       `yaml:"-"` // 是否存在同目录的gocqhttp
	InPackGoCqHttpLoginSuccess bool                       `yaml:"-"` // 是否登录成功
	InPackGoCqHttpRunning      bool                       `yaml:"-"` // 是否仍在运行
	TextMapRaw                 TextTemplateWithWeightDict `yaml:"-"`
	TextMapHelpInfo            TextTemplateWithHelpDict   `yaml:"-"`
	Parent                     *DiceManager               `yaml:"-"`
}

func (d *Dice) Init() {
	d.BaseConfig.DataDir = filepath.Join("./data", d.BaseConfig.Name)
	os.MkdirAll(d.BaseConfig.DataDir, 0644)
	os.MkdirAll(filepath.Join(d.BaseConfig.DataDir, "configs"), 0644)
	os.MkdirAll(filepath.Join(d.BaseConfig.DataDir, "extensions"), 0644)
	os.MkdirAll(filepath.Join(d.BaseConfig.DataDir, "logs"), 0644)
	os.MkdirAll(filepath.Join(d.BaseConfig.DataDir, "extra"), 0644)

	d.DB = model.BoltDBInit(filepath.Join(d.BaseConfig.DataDir, "data.bdb"))
	log := logger.LoggerInit(filepath.Join(d.BaseConfig.DataDir, "record.log"), d.BaseConfig.Name, d.BaseConfig.IsLogPrint)
	d.Logger = log.Logger
	d.LogWriter = log.WX

	d.CommandCompatibleMode = true
	d.ImSession = &IMSession{}
	d.ImSession.Parent = d
	d.ImSession.ServiceAt = make(map[int64]*ServiceAtItem)
	d.CmdMap = CmdMapCls{}

	d.registerCoreCommands()
	d.RegisterBuiltinExt()
	d.loads()

	for _, i := range d.ExtList {
		if i.OnLoad != nil {
			i.OnLoad()
		}
	}

	autoSave := func() {
		t := time.Tick(30 * time.Second)
		for {
			<-t
			d.Save(true)
		}
	}
	go autoSave()

	refreshGroupInfo := func() {
		t := time.Tick(35 * time.Second)
		defer func() {
			// 防止报错
			if r := recover(); r != nil {
				d.Logger.Error(r)
			}
		}()

		for {
			<-t

			// 自动更新群信息
			for _, i := range d.ImSession.Conns {
				if i.Enable && i.DiceServing {
					for k, v := range d.ImSession.ServiceAt {
						if v.Active {
							diceId := FormatDiceIdQQ(i.UserId)
							if len(v.DiceIds) == 0 {
								v.DiceIds[diceId] = true
							}
							if v.DiceIds[diceId] {
								GetGroupInfo(i.Socket, k)
							}
						}
					}
				}
			}
		}
	}
	go refreshGroupInfo()
}

func (d *Dice) rebuildParser(buffer string) *DiceRollParser {
	p := &DiceRollParser{Buffer: buffer}
	_ = p.Init()
	p.RollExpression.Init(255)
	//d.RollParser = p;
	return p
}

func (d *Dice) ExprEvalBase(buffer string, ctx *MsgContext, flags RollExtraFlags) (*VmResult, string, error) {
	parser := d.rebuildParser(buffer)
	err := parser.Parse()
	parser.RollExpression.flags = flags

	if err == nil {
		parser.Execute()
		if parser.Error != nil {
			return nil, "", parser.Error
		}
		num, detail, err := parser.Evaluate(d, ctx)
		if err != nil {
			return nil, "", err
		}

		ret := VmResult{}
		ret.Value = num.Value
		ret.TypeId = num.TypeId
		ret.Parser = parser

		tks := parser.Tokens()
		// 注意，golang的string下标等同于[]byte下标，也就是说中文会被打断
		// parser里有一个[]rune类型的，但问题是他句尾带了一个endsymbol
		runeBuffer := []rune(buffer)
		lastToken := tks[len(tks)-1]
		ret.restInput = strings.TrimSpace(string(runeBuffer[lastToken.end:]))
		ret.Matched = strings.TrimSpace(string(runeBuffer[:lastToken.end]))
		return &ret, detail, nil
	}
	return nil, "", err
}

func (d *Dice) ExprEval(buffer string, ctx *MsgContext) (*VmResult, string, error) {
	return d.ExprEvalBase(buffer, ctx, RollExtraFlags{})
}

func (d *Dice) ExprTextBase(buffer string, ctx *MsgContext) (*VmResult, string, error) {
	buffer = strings.ReplaceAll(buffer, "`", "")
	val, detail, err := d.ExprEval("`"+buffer+"`", ctx)

	if err == nil && (val.TypeId == VMTypeString || val.TypeId == VMTypeNone) {
		return val, detail, err
	}

	return nil, "", errors.New("错误的表达式")
}

func (d *Dice) ExprText(buffer string, ctx *MsgContext) (string, string, error) {
	val, detail, err := d.ExprTextBase(buffer, ctx)

	if err == nil && (val.TypeId == VMTypeString || val.TypeId == VMTypeNone) {
		return val.Value.(string), detail, err
	}

	return "", "", errors.New("错误的表达式")
}

func DiceRoll(dicePoints int) int {
	if dicePoints <= 0 {
		return 0
	}
	val := rand.Int()%dicePoints + 1
	return val
}

func DiceRoll64(dicePoints int64) int64 {
	if dicePoints == 0 {
		return 0
	}
	val := rand.Int63()%dicePoints + 1
	return val
}
