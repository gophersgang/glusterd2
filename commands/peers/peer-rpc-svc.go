package peercommands

import (
	"fmt"

	"github.com/gluster/glusterd2/etcdmgmt"
	"github.com/gluster/glusterd2/gdctx"
	"github.com/gluster/glusterd2/peer"
	"github.com/gluster/glusterd2/servers/peerrpc"
	"github.com/gluster/glusterd2/volume"

	log "github.com/Sirupsen/logrus"
	config "github.com/spf13/viper"
	netctx "golang.org/x/net/context"
	"google.golang.org/grpc"
)

// PeerService will be handling client requests on the server side for peer ops
type PeerService int

func init() {
	peerrpc.Register(new(PeerService))
}

// RegisterService registers a service
func (p *PeerService) RegisterService(s *grpc.Server) {
	RegisterPeerServiceServer(s, p)
}

var (
	etcdConfDir  = "/var/lib/glusterd/"
	etcdConfFile = etcdConfDir + "etcdenv.conf"
)

// ValidateAdd validates AddPeer operation at server side
func (p *PeerService) ValidateAdd(nc netctx.Context, args *PeerAddReq) (*PeerAddResp, error) {
	var opRet int32
	var opError string
	uuid := gdctx.MyUUID.String()

	if gdctx.MaxOpVersion < 40000 {
		opRet = -1
		opError = fmt.Sprintf("GlusterD instance running on %s is not compatible", args.Name)
	}
	volumes, _ := volume.GetVolumes()
	if len(volumes) != 0 {
		opRet = -1
		opError = fmt.Sprintf("Peer %s already has existing volumes", args.Name)
	}

	reply := &PeerAddResp{
		OpRet:           opRet,
		OpError:         opError,
		UUID:            uuid,
		PeerName:        gdctx.HostName,
		EtcdPeerAddress: config.GetString("etcdpeeraddress"),
	}
	return reply, nil
}

// ValidateDelete validates DeletePeer operation at server side
func (p *PeerService) ValidateDelete(nc netctx.Context, args *PeerDeleteReq) (*PeerGenericResp, error) {
	var opRet int32
	var opError string
	// TODO : Validate if this guy has any volume configured where the brick(s) is
	// hosted in some other node, in that case the validation should fail

	reply := &PeerGenericResp{
		OpRet:   opRet,
		OpError: opError,
	}
	return reply, nil
}

// ExportAndStoreETCDConfig will store & export etcd environment variable along
// with storing etcd configuration
func (p *PeerService) ExportAndStoreETCDConfig(nc netctx.Context, c *EtcdConfigReq) (*PeerGenericResp, error) {
	var opRet int32
	var opError string

	// Stop the store first
	gdctx.Store.Close()

	newEtcdConfig, err := etcdmgmt.GetEtcdConfig(false)
	if err != nil {
		opRet = -1
		opError = fmt.Sprintf("Could not fetch etcd configuration.")
		goto Out
	}

	if !c.DeletePeer {
		// This is an add peer request containing information about
		// which cluster to join.
		newEtcdConfig.InitialCluster = c.InitialCluster
		newEtcdConfig.ClusterState = c.ClusterState
		newEtcdConfig.Name = c.EtcdName
		newEtcdConfig.Dir = newEtcdConfig.Name + ".etcd"
	}

	// Gracefully stop embedded etcd server and remove local etcd data
	err = etcdmgmt.DestroyEmbeddedEtcd(true)
	if err != nil {
		opRet = -1
		opError = fmt.Sprintf("Error stopping embedded etcd server.")
		log.WithField("Error", err).Error("Error stopping embedded etcd server.")
		goto Out
	}

	// Start embedded etcd server
	err = etcdmgmt.StartEmbeddedEtcd(newEtcdConfig)
	if err != nil {
		opRet = -1
		opError = fmt.Sprintf("Could not start embedded etcd server.")
		log.WithField("Error", err).Error("Could not start embedded etcd server.")
		goto Out
	}

	// Reinitialize the store now that a new etcd instance is running
	gdctx.InitStore()

	if c.DeletePeer {
		// After being detached from the cluster, this glusterd instance
		// now should get back to clean slate i.e state of a single node
		// standalone cluster.
		peer.AddSelfDetails()
	} else {
		// Store the etcd config in a file for use during restarts.
		err = etcdmgmt.StoreEtcdConfig(newEtcdConfig)
		if err != nil {
			opRet = -1
			opError = fmt.Sprintf("Error storing etcd configuration.")
			goto Out
		}
	}

Out:
	reply := &PeerGenericResp{
		OpRet:   opRet,
		OpError: opError,
	}

	return reply, nil
}
