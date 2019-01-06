package bastion // import "github.com/doovemax/sshportal/pkg/bastion"

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/moul/ssh"
	gossh "golang.org/x/crypto/ssh"
	"github.com/doovemax/sshportal/pkg/crypto"
	"github.com/doovemax/sshportal/pkg/dbmodels"
)

type sshportalContextKey string

var authContextKey = sshportalContextKey("auth")

type authContext struct {
	message         string
	err             error
	user            dbmodels.User
	inputUsername   string
	db              *gorm.DB
	userKey         dbmodels.UserKey
	logsLocation    string
	aesKey          string
	dbDriver, dbURL string
	bindAddr        string
	demo, debug     bool
	authMethod      string
	authSuccess     bool
}

type userType string

const (
	userTypeHealthcheck userType = "healthcheck"
	userTypeBastion     userType = "bastion"
	userTypeInvite      userType = "invite"
	userTypeShell       userType = "shell"
)

func (c authContext) userType() userType {
	switch {
	case c.inputUsername == "healthcheck":
		return userTypeHealthcheck
	case c.inputUsername == c.user.Name || c.inputUsername == c.user.Email || c.inputUsername == "admin":
		return userTypeShell
	case strings.HasPrefix(c.inputUsername, "invite:"):
		return userTypeInvite
	default:
		return userTypeBastion
	}
}

func dynamicHostKey(db *gorm.DB, host *dbmodels.Host) gossh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		if len(host.HostKey) == 0 {
			log.Println("Discovering host fingerprint...")
			return db.Model(host).Update("HostKey", key.Marshal()).Error
		}

		if !bytes.Equal(host.HostKey, key.Marshal()) {
			return fmt.Errorf("ssh: host key mismatch")
		}
		return nil
	}
}

var DefaultChannelHandler ssh.ChannelHandler = func(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {}

func ChannelHandler(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
	switch newChan.ChannelType() {
	case "session":
	case "direct-tcpip":
	default:
		// TODO: handle direct-tcp (only for ssh scheme)
		if err := newChan.Reject(gossh.UnknownChannelType, "unsupported channel type"); err != nil {
			log.Printf("error: failed to reject channel: %v", err)
		}
		return
	}

	actx := ctx.Value(authContextKey).(*authContext)

	switch actx.userType() {
	case userTypeBastion:
		log.Printf("New connection(bastion): sshUser=%q remote=%q local=%q dbUser=id:%q,email:%s", conn.User(), conn.RemoteAddr(), conn.LocalAddr(), actx.user.ID, actx.user.Email)
		host, err := dbmodels.HostByName(actx.db, actx.inputUsername)
		if err != nil {
			ch, _, err2 := newChan.Accept()
			if err2 != nil {
				return
			}
			fmt.Fprintf(ch, "error: %v\n", err)
			// FIXME: force close all channels
			_ = ch.Close()
			return
		}

		switch host.Scheme() {
		case dbmodels.BastionSchemeSSH:
			sessionConfigs := make([]sessionConfig, 0)
			currentHost := host
			for currentHost != nil {
				clientConfig, err2 := bastionClientConfig(ctx, currentHost)
				if err2 != nil {
					ch, _, err3 := newChan.Accept()
					if err3 != nil {
						return
					}
					fmt.Fprintf(ch, "error: %v\n", err2)
					// FIXME: force close all channels
					_ = ch.Close()
					return
				}
				sessionConfigs = append([]sessionConfig{{
					Addr:         currentHost.DialAddr(),
					ClientConfig: clientConfig,
					Logs:         actx.logsLocation,
				}}, sessionConfigs...)
				if currentHost.HopID != 0 {
					var newHost dbmodels.Host
					actx.db.Model(currentHost).Related(&newHost, "HopID")
					hostname := newHost.Name
					currentHost, _ = dbmodels.HostByName(actx.db, hostname)
				} else {
					currentHost = nil
				}
			}

			sess := dbmodels.Session{
				UserID: actx.user.ID,
				HostID: host.ID,
				Status: string(dbmodels.SessionStatusActive),
			}
			if err = actx.db.Create(&sess).Error; err != nil {
				ch, _, err2 := newChan.Accept()
				if err2 != nil {
					return
				}
				fmt.Fprintf(ch, "error: %v\n", err)
				_ = ch.Close()
				return
			}

			go func() {
				err = multiChannelHandler(srv, conn, newChan, ctx, sessionConfigs)
				if err != nil {
					log.Printf("Error: %v", err)
				}

				now := time.Now()
				sessUpdate := dbmodels.Session{
					Status:    string(dbmodels.SessionStatusClosed),
					ErrMsg:    fmt.Sprintf("%v", err),
					StoppedAt: &now,
				}
				switch sessUpdate.ErrMsg {
				case "lch closed the connection", "rch closed the connection":
					sessUpdate.ErrMsg = ""
				}
				actx.db.Model(&sess).Updates(&sessUpdate)
			}()
		case dbmodels.BastionSchemeTelnet:
			tmpSrv := ssh.Server{
				// PtyCallback: srv.PtyCallback,
				Handler: telnetHandler(host),
			}
			DefaultChannelHandler(&tmpSrv, conn, newChan, ctx)
		default:
			ch, _, err2 := newChan.Accept()
			if err2 != nil {
				return
			}
			fmt.Fprintf(ch, "error: unknown bastion scheme: %q\n", host.Scheme())
			// FIXME: force close all channels
			_ = ch.Close()
		}
	default: // shell
		DefaultChannelHandler(srv, conn, newChan, ctx)
	}
}

func bastionClientConfig(ctx ssh.Context, host *dbmodels.Host) (*gossh.ClientConfig, error) {
	actx := ctx.Value(authContextKey).(*authContext)

	clientConfig, err := host.ClientConfig(dynamicHostKey(actx.db, host))
	if err != nil {
		return nil, err
	}

	var tmpUser dbmodels.User
	if err = actx.db.Preload("Groups").Preload("Groups.ACLs").Where("id = ?", actx.user.ID).First(&tmpUser).Error; err != nil {
		return nil, err
	}
	var tmpHost dbmodels.Host
	if err = actx.db.Preload("Groups").Preload("Groups.ACLs").Where("id = ?", host.ID).First(&tmpHost).Error; err != nil {
		return nil, err
	}
	action, err2 := checkACLs(tmpUser, tmpHost)
	if err2 != nil {
		return nil, err2
	}

	crypto.HostDecrypt(actx.aesKey, host)
	crypto.SSHKeyDecrypt(actx.aesKey, host.SSHKey)

	switch action {
	case string(dbmodels.ACLActionAllow):
	case string(dbmodels.ACLActionDeny):
		return nil, fmt.Errorf("you don't have permission to that host")
	default:
		return nil, fmt.Errorf("invalid ACL action: %q", action)
	}
	return clientConfig, nil
}

func ShellHandler(s ssh.Session, version, gitSha, gitTag, gitBranch string) {
	actx := s.Context().Value(authContextKey).(*authContext)
	if actx.userType() != userTypeHealthcheck {
		log.Printf("New connection(shell): sshUser=%q remote=%q local=%q command=%q dbUser=id:%q,email:%s", s.User(), s.RemoteAddr(), s.LocalAddr(), s.Command(), actx.user.ID, actx.user.Email)
	}

	if actx.err != nil {
		fmt.Fprintf(s, "error: %v\n", actx.err)
		_ = s.Exit(1)
		return
	}

	if actx.message != "" {
		fmt.Fprint(s, actx.message)
	}

	switch actx.userType() {
	case userTypeHealthcheck:
		fmt.Fprintln(s, "OK")
		return
	case userTypeShell:
		if err := shell(s, version, gitSha, gitTag, gitBranch); err != nil {
			fmt.Fprintf(s, "error: %v\n", err)
			_ = s.Exit(1)
		}
		return
	case userTypeInvite:
		// do nothing (message was printed at the beginning of the function)
		return
	}
	panic("should not happen")
}

func PasswordAuthHandler(db *gorm.DB, logsLocation, aesKey, dbDriver, dbURL, bindAddr string, demo bool) ssh.PasswordHandler {
	return func(ctx ssh.Context, pass string) bool {
		actx := &authContext{
			db:            db,
			inputUsername: ctx.User(),
			logsLocation:  logsLocation,
			aesKey:        aesKey,
			dbDriver:      dbDriver,
			dbURL:         dbURL,
			bindAddr:      bindAddr,
			demo:          demo,
			authMethod:    "password",
		}
		actx.authSuccess = actx.userType() == userTypeHealthcheck
		ctx.SetValue(authContextKey, actx)
		return actx.authSuccess
	}
}

func PrivateKeyFromDB(db *gorm.DB, aesKey string) func(*ssh.Server) error {
	return func(srv *ssh.Server) error {
		var key dbmodels.SSHKey
		if err := dbmodels.SSHKeysByIdentifiers(db, []string{"host"}).First(&key).Error; err != nil {
			return err
		}
		crypto.SSHKeyDecrypt(aesKey, &key)

		signer, err := gossh.ParsePrivateKey([]byte(key.PrivKey))
		if err != nil {
			return err
		}
		srv.AddHostKey(signer)
		return nil
	}
}

func PublicKeyAuthHandler(db *gorm.DB, logsLocation, aesKey, dbDriver, dbURL, bindAddr string, demo bool) ssh.PublicKeyHandler {
	return func(ctx ssh.Context, key ssh.PublicKey) bool {
		actx := &authContext{
			db:            db,
			inputUsername: ctx.User(),
			logsLocation:  logsLocation,
			aesKey:        aesKey,
			dbDriver:      dbDriver,
			dbURL:         dbURL,
			bindAddr:      bindAddr,
			demo:          demo,
			authMethod:    "pubkey",
			authSuccess:   true,
		}
		ctx.SetValue(authContextKey, actx)

		// lookup user by key
		db.Where("authorized_key = ?", string(gossh.MarshalAuthorizedKey(key))).First(&actx.userKey)
		if actx.userKey.UserID > 0 {
			db.Preload("Roles").Where("id = ?", actx.userKey.UserID).First(&actx.user)
			if actx.userType() == userTypeInvite {
				actx.err = fmt.Errorf("invites are only supported for new SSH keys; your ssh key is already associated with the user %q", actx.user.Email)
			}
			return true
		}

		// handle invite "links"
		if actx.userType() == userTypeInvite {
			inputToken := strings.Split(actx.inputUsername, ":")[1]
			if len(inputToken) > 0 {
				db.Where("invite_token = ?", inputToken).First(&actx.user)
			}
			if actx.user.ID > 0 {
				actx.userKey = dbmodels.UserKey{
					UserID:        actx.user.ID,
					Key:           key.Marshal(),
					Comment:       "created by sshportal",
					AuthorizedKey: string(gossh.MarshalAuthorizedKey(key)),
				}
				db.Create(&actx.userKey)

				// token is only usable once
				actx.user.InviteToken = ""
				db.Model(&actx.user).Updates(&actx.user)

				actx.message = fmt.Sprintf("Welcome %s!\n\nYour key is now associated with the user %q.\n", actx.user.Name, actx.user.Email)
			} else {
				actx.user = dbmodels.User{Name: "Anonymous"}
				actx.err = errors.New("your token is invalid or expired")
			}
			return true
		}

		// fallback
		actx.err = errors.New("unknown ssh key")
		actx.user = dbmodels.User{Name: "Anonymous"}
		return true
	}
}
