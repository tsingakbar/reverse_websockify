// Code generated by the FlatBuffers compiler. DO NOT EDIT.

package ReverseWebsockify

import "strconv"

type Action int8

const (
	ActionConnect     Action = 0
	ActionEstablished Action = 1
	ActionForward     Action = 2
	ActionClose       Action = 3
)

var EnumNamesAction = map[Action]string{
	ActionConnect:     "Connect",
	ActionEstablished: "Established",
	ActionForward:     "Forward",
	ActionClose:       "Close",
}

var EnumValuesAction = map[string]Action{
	"Connect":     ActionConnect,
	"Established": ActionEstablished,
	"Forward":     ActionForward,
	"Close":       ActionClose,
}

func (v Action) String() string {
	if s, ok := EnumNamesAction[v]; ok {
		return s
	}
	return "Action(" + strconv.FormatInt(int64(v), 10) + ")"
}
