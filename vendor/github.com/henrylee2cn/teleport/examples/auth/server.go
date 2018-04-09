package main

import (
	tp "github.com/henrylee2cn/teleport"
	"github.com/henrylee2cn/teleport/plugin"
)

func main() {
	svr := tp.NewPeer(tp.PeerConfig{
		ListenAddress: ":9090",
	}, plugin.VerifyAuth(verifyAuthInfo))
	svr.Listen()
}

const (
	clientAuthInfo = "client-auth-info-12345"
	codeAuthFail   = 403
	textAuthFail   = "auth fail"
	detailAuthFail = "auth fail detail"
)

func verifyAuthInfo(authInfo string, sess plugin.AuthSession) *tp.Rerror {
	tp.Infof("auth info: %v", authInfo)
	if clientAuthInfo != authInfo {
		return tp.NewRerror(codeAuthFail, textAuthFail, detailAuthFail)
	}
	return nil
}
