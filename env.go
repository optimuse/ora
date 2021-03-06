// Copyright 2014 Rana Ian. All rights reserved.
// Use of this source code is governed by The MIT License
// found in the accompanying LICENSE file.

package ora

/*
#include <oci.h>
#include <stdlib.h>
*/
import "C"
import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

// EnvCfg configures a new Env.
type EnvCfg struct {
	// StmtCfg configures new Stmts.
	StmtCfg *StmtCfg
}

// NewEnvCfg creates a EnvCfg with default values.
func NewEnvCfg() *EnvCfg {
	c := &EnvCfg{}
	c.StmtCfg = NewStmtCfg()
	return c
}

// LogEnvCfg represents Env logging configuration values.
type LogEnvCfg struct {
	// Close determines whether the Env.Close method is logged.
	//
	// The default is true.
	Close bool

	// OpenSrv determines whether the Env.OpenSrv method is logged.
	//
	// The default is true.
	OpenSrv bool

	// OpenCon determines whether the Env.OpenCon method is logged.
	//
	// The default is true.
	OpenCon bool
}

// NewLogEnvCfg creates a LogEnvCfg with default values.
func NewLogEnvCfg() LogEnvCfg {
	c := LogEnvCfg{}
	c.Close = true
	c.OpenSrv = true
	c.OpenCon = true
	return c
}

// Env represents an Oracle environment.
type Env struct {
	id     uint64
	cfg    EnvCfg
	mu     sync.Mutex
	ocienv *C.OCIEnv
	ocierr *C.OCIError
	errBuf [512]C.char

	openSrvs *list.List
	openCons *list.List
	elem     *list.Element
}

// Close disconnects from servers and resets optional fields.
func (env *Env) Close() (err error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.log(_drv.cfg.Log.Env.Close)
	err = env.checkClosed()
	if err != nil {
		return errE(err)
	}
	errs := _drv.listPool.Get().(*list.List)
	defer func() {
		if value := recover(); value != nil {
			errs.PushBack(errR(value))
		}
		_drv.openEnvs.Remove(env.elem)
		env.ocienv = nil
		env.ocierr = nil
		env.elem = nil
		env.openSrvs.Init()
		env.openCons.Init()
		_drv.envPool.Put(env)

		multiErr := newMultiErrL(errs)
		if multiErr != nil {
			err = errE(*multiErr)
		}
		errs.Init()
		_drv.listPool.Put(errs)
	}()
	for e := env.openCons.Front(); e != nil; e = e.Next() { // close connections
		err = e.Value.(*Con).Close()
		if err != nil {
			errs.PushBack(errE(err))
		}
	}
	for e := env.openSrvs.Front(); e != nil; e = e.Next() { // close servers
		err = e.Value.(*Srv).Close()
		if err != nil {
			errs.PushBack(errE(err))
		}
	}
	// Free oci environment handle and all oci child handles
	// The oci error handle is released as a child of the environment handle
	err = env.freeOciHandle(unsafe.Pointer(env.ocienv), C.OCI_HTYPE_ENV)
	if err != nil {
		return errE(err)
	}
	return nil
}

// OpenSrv connects to an Oracle server returning a *Srv and possible error.
func (env *Env) OpenSrv(cfg *SrvCfg) (srv *Srv, err error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.log(_drv.cfg.Log.Env.OpenSrv)
	err = env.checkClosed()
	if err != nil {
		return nil, errE(err)
	}
	if cfg == nil {
		return nil, er("Parameter 'cfg' may not be nil.")
	}
	// allocate server handle
	ocisrv, err := env.allocOciHandle(C.OCI_HTYPE_SERVER)
	if err != nil {
		return nil, errE(err)
	}
	// attach to server
	cDblink := C.CString(cfg.Dblink)
	defer C.free(unsafe.Pointer(cDblink))
	r := C.OCIServerAttach(
		(*C.OCIServer)(ocisrv),                //OCIServer     *srvhp,
		env.ocierr,                            //OCIError      *errhp,
		(*C.OraText)(unsafe.Pointer(cDblink)), //const OraText *dblink,
		C.sb4(len(cfg.Dblink)),                //sb4           dblink_len,
		C.OCI_DEFAULT)                         //ub4           mode);
	if r == C.OCI_ERROR {
		return nil, errE(env.ociError())
	}
	// allocate service context handle
	ocisvcctx, err := env.allocOciHandle(C.OCI_HTYPE_SVCCTX)
	if err != nil {
		return nil, errE(err)
	}
	// set server handle onto service context handle
	err = env.setAttr(ocisvcctx, C.OCI_HTYPE_SVCCTX, ocisrv, C.ub4(0), C.OCI_ATTR_SERVER)
	if err != nil {
		return nil, errE(err)
	}

	srv = _drv.srvPool.Get().(*Srv) // set *Srv
	srv.env = env
	srv.ocisrv = (*C.OCIServer)(ocisrv)
	srv.ocisvcctx = (*C.OCISvcCtx)(ocisvcctx)
	srv.elem = env.openSrvs.PushBack(srv)
	if srv.id == 0 {
		srv.id = _drv.srvId.nextId()
	}
	srv.cfg = *cfg
	if srv.cfg.StmtCfg == nil && srv.env.cfg.StmtCfg != nil {
		srv.cfg.StmtCfg = &(*srv.env.cfg.StmtCfg) // copy by value so that user may change independently
	}
	return srv, nil
}

var (
	conCharset   = make(map[string]string, 2)
	conCharsetMu sync.Mutex
)

// OpenCon starts an Oracle session on a server returning a *Con and possible error.
//
// The connection string has the form username/password@dblink e.g., scott/tiger@orcl
// dblink is a connection identifier such as a net service name,
// full connection identifier, or a simple connection identifier.
// The dblink may be defined in the client machine's tnsnames.ora file.
func (env *Env) OpenCon(str string) (con *Con, err error) {
	// do not lock; calls to env.OpenSrv will lock
	env.log(_drv.cfg.Log.Env.OpenCon)
	err = env.checkClosed()
	if err != nil {
		return nil, errE(err)
	}
	// parse connection string
	var username string
	var password string
	var dblink string
	str = strings.TrimSpace(str)
	if strings.HasPrefix(str, "/@") {
		dblink = str[2:]
	} else {
		str = strings.Replace(str, "/", " / ", 1)
		str = strings.Replace(str, "@", " @ ", 1)
		_, err := fmt.Sscanf(str, "%s / %s @ %s", &username, &password, &dblink)
		if err != nil {
			return nil, errE(err)
		}
	}
	srvCfg := NewSrvCfg()
	srvCfg.Dblink = dblink
	srv, err := env.OpenSrv(srvCfg) // open Srv
	if err != nil {
		return nil, errE(err)
	}
	sesCfg := NewSesCfg()
	sesCfg.Username = username
	sesCfg.Password = password
	sesCfg.StmtCfg = srv.env.cfg.StmtCfg // sqlPkg StmtCfg has been configured for database/sql package
	ses, err := srv.OpenSes(sesCfg)      // open Ses
	if err != nil {
		return nil, errE(err)
	}
	con = _drv.conPool.Get().(*Con) // set *Con
	con.env = env
	con.srv = srv
	con.ses = ses
	con.elem = env.openCons.PushBack(con)
	if con.id == 0 {
		con.id = _drv.conId.nextId()
	}
	conCharsetMu.Lock()
	defer conCharsetMu.Unlock()
	if cs, ok := conCharset[dblink]; ok {
		srv.dbIsUTF8 = cs == "AL32UTF8"
		return con, nil
	}
	if rset, err := ses.PrepAndQry(
		`SELECT property_value FROM database_properties WHERE property_name = 'NLS_CHARACTERSET'`,
	); err != nil {
		//Log.Errorf("E%vS%vS%v] Determine database characterset: %v",
		//	env.id, con.id, ses.id, err)
	} else if rset != nil && rset.Next() && len(rset.Row) == 1 {
		//Log.Infof("E%vS%vS%v] Database characterset=%q",
		//	env.id, con.id, ses.id, rset.Row[0])
		if cs, ok := rset.Row[0].(string); ok {
			conCharset[dblink] = cs
			con.srv.dbIsUTF8 = cs == "AL32UTF8"
		}
	}
	return con, nil
}

// NumSrv returns the number of open Oracle servers.
func (env *Env) NumSrv() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.openSrvs.Len()
}

// NumCon returns the number of open Oracle connections.
func (env *Env) NumCon() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.openCons.Len()
}

// SetCfg applies the specified cfg to the Env.
//
// Open Srvs do not observe the specified cfg.
func (env *Env) SetCfg(cfg *EnvCfg) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.cfg = *cfg
}

// Cfg returns the Env's cfg.
func (env *Env) Cfg() *EnvCfg {
	env.mu.Lock()
	defer env.mu.Unlock()
	return &env.cfg
}

// IsOpen returns true when the environment is open; otherwise, false.
//
// Calling Close will cause IsOpen to return false. Once closed, the environment
// may be re-opened by calling Open.
func (env *Env) IsOpen() bool {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.ocienv != nil
}

// checkClosed returns an error if Env is closed. No locking occurs.
func (env *Env) checkClosed() error {
	if env == nil || env.ocienv == nil {
		return er("Env is closed.")
	}
	return nil
}

// sysName returns a string representing the Env.
func (env *Env) sysName() string {
	return fmt.Sprintf("E%v", env.id)
}

// log writes a message with an Env system name and caller info.
func (env *Env) log(enabled bool, v ...interface{}) {
	if enabled {
		if len(v) == 0 {
			_drv.cfg.Log.Logger.Infof("%v %v", env.sysName(), callInfo(1))
		} else {
			_drv.cfg.Log.Logger.Infof("%v %v %v", env.sysName(), callInfo(1), fmt.Sprint(v...))
		}
	}
}

// log writes a formatted message with an Env system name and caller info.
func (env *Env) logF(enabled bool, format string, v ...interface{}) {
	if enabled {
		if len(v) == 0 {
			_drv.cfg.Log.Logger.Infof("%v %v", env.sysName(), callInfo(1))
		} else {
			_drv.cfg.Log.Logger.Infof("%v %v %v", env.sysName(), callInfo(1), fmt.Sprintf(format, v...))
		}
	}
}

// allocateOciHandle allocates an oci handle. No locking occurs.
func (env *Env) allocOciHandle(handleType C.ub4) (unsafe.Pointer, error) {
	// OCIHandleAlloc returns: OCI_SUCCESS, OCI_INVALID_HANDLE
	var handle unsafe.Pointer
	r := C.OCIHandleAlloc(
		unsafe.Pointer(env.ocienv), //const void    *parenth,
		&handle,                    //void          **hndlpp,
		handleType,                 //ub4           type,
		C.size_t(0),                //size_t        xtramem_sz,
		nil)                        //void          **usrmempp
	if r == C.OCI_INVALID_HANDLE {
		return nil, er("Unable to allocate handle")
	}
	return handle, nil
}

// freeOciHandle deallocates an oci handle. No locking occurs.
func (env *Env) freeOciHandle(ociHandle unsafe.Pointer, handleType C.ub4) error {
	// OCIHandleFree returns: OCI_SUCCESS, OCI_INVALID_HANDLE, or OCI_ERROR
	r := C.OCIHandleFree(
		ociHandle,  //void      *hndlp,
		handleType) //ub4       type );
	if r == C.OCI_INVALID_HANDLE {
		return er("Unable to free handle")
	} else if r == C.OCI_ERROR {
		return errE(env.ociError())
	}
	return nil
}

// setOciAttribute sets an attribute value on a handle or descriptor. No locking occurs.
func (env *Env) setAttr(
	target unsafe.Pointer,
	targetType C.ub4,
	attribute unsafe.Pointer,
	attributeSize C.ub4,
	attributeType C.ub4) (err error) {

	r := C.OCIAttrSet(
		target,        //void        *trgthndlp,
		targetType,    //ub4         trghndltyp,
		attribute,     //void        *attributep,
		attributeSize, //ub4         size,
		attributeType, //ub4         attrtype,
		env.ocierr)    //OCIError    *errhp );
	if r == C.OCI_ERROR {
		return errE(env.ociError())
	}
	return nil
}

// getOciError gets an error returned by an Oracle server. No locking occurs.
func (env *Env) ociError() error {
	var errcode C.sb4
	C.OCIErrorGet(
		unsafe.Pointer(env.ocierr),
		1, nil,
		&errcode,
		(*C.OraText)(unsafe.Pointer(&env.errBuf[0])),
		C.ub4(len(env.errBuf)),
		C.OCI_HTYPE_ERROR)
	return er(C.GoString(&env.errBuf[0]))
}
