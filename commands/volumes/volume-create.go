package volumecommands

import (
	"errors"
	"net/http"

	gderrors "github.com/gluster/glusterd2/errors"
	"github.com/gluster/glusterd2/peer"
	restutils "github.com/gluster/glusterd2/servers/rest/utils"
	"github.com/gluster/glusterd2/transaction"
	"github.com/gluster/glusterd2/utils"
	"github.com/gluster/glusterd2/volgen"
	"github.com/gluster/glusterd2/volume"

	log "github.com/Sirupsen/logrus"
	"github.com/pborman/uuid"
)

func unmarshalVolCreateRequest(msg *volume.VolCreateRequest, r *http.Request) (int, error) {
	e := utils.GetJSONFromRequest(r, msg)
	if e != nil {
		log.WithField("error", e).Error("Failed to parse the JSON Request")
		return 422, gderrors.ErrJSONParsingFailed
	}

	if msg.Name == "" {
		log.Error("Volume name is empty")
		return http.StatusBadRequest, gderrors.ErrEmptyVolName
	}
	if len(msg.Bricks) <= 0 {
		log.WithField("volume", msg.Name).Error("Brick list is empty")
		return http.StatusBadRequest, gderrors.ErrEmptyBrickList
	}
	return 0, nil

}

func createVolinfo(msg *volume.VolCreateRequest) (*volume.Volinfo, error) {
	vol, err := volume.NewVolumeEntry(msg)
	if err != nil {
		return nil, err
	}
	vol.Bricks, err = volume.NewBrickEntriesFunc(msg.Bricks, vol.Name)
	if err != nil {
		return nil, err
	}
	return vol, nil
}

func validateVolumeCreate(c transaction.TxnCtx) error {

	var req volume.VolCreateRequest
	err := c.Get("req", &req)
	if err != nil {
		return err
	}

	var vol volume.Volinfo
	err = c.Get("volinfo", &vol)
	if err != nil {
		return err
	}

	// FIXME: Return values of this function are inconsistent and unused
	_, err = volume.ValidateBrickEntriesFunc(vol.Bricks, vol.ID, req.Force)
	if err != nil {
		return err
	}

	return nil
}

func generateVolfiles(c transaction.TxnCtx) error {
	var vol volume.Volinfo
	e := c.Get("volinfo", &vol)
	if e != nil {
		return errors.New("generateVolfiles: Failed to get volinfo from context")
	}

	var volAuth volume.VolAuth
	e = c.Get("volauth", &volAuth)
	if e != nil {
		return errors.New("generateVolfiles: Failed to get volauth from context")
	}

	// Creating client and server volfile
	e = volgen.GenerateVolfileFunc(&vol, &volAuth)
	if e != nil {
		c.Logger().WithFields(log.Fields{"error": e.Error(),
			"volume": vol.Name,
		}).Error("failed to generate volfile")
		return e
	}
	return nil
}

func storeVolume(c transaction.TxnCtx) error {
	var vol volume.Volinfo
	e := c.Get("volinfo", &vol)
	if e != nil {
		return errors.New("failed to get volinfo from context")
	}

	e = volume.AddOrUpdateVolumeFunc(&vol)
	if e != nil {
		c.Logger().WithFields(log.Fields{"error": e.Error(),
			"volume": vol.Name,
		}).Error("Failed to create volume")
		return e
	}

	log.WithField("volume", vol.Name).Debug("new volume added")
	return nil
}

func rollBackVolumeCreate(c transaction.TxnCtx) error {
	var vol volume.Volinfo
	e := c.Get("volinfo", &vol)
	if e != nil {
		return errors.New("failed to get volinfo from context")
	}

	_ = volume.RemoveBrickPaths(vol.Bricks)

	return nil
}

func registerVolCreateStepFuncs() {
	var sfs = []struct {
		name string
		sf   transaction.StepFunc
	}{
		{"vol-create.Stage", validateVolumeCreate},
		{"vol-create.Commit", generateVolfiles},
		{"vol-create.Store", storeVolume},
		{"vol-create.Rollback", rollBackVolumeCreate},
	}
	for _, sf := range sfs {
		transaction.RegisterStepFunc(sf.sf, sf.name)
	}
}

// nodesForVolCreate returns a list of Nodes which volume create touches
func nodesForVolCreate(req *volume.VolCreateRequest) ([]uuid.UUID, error) {
	var nodes []uuid.UUID

	for _, b := range req.Bricks {

		// Bricks specified can have one of the following formats:
		// <peer-uuid>:<brick-path>
		// <ip>:<port>:<brick-path>
		// <ip>:<brick-path>
		// TODO: Peer names, as of today, aren't unique. Support it ?
		// TODO: Change API to have host and path as separate fields

		host, _, err := utils.ParseHostAndBrickPath(b)
		if err != nil {
			return nil, err
		}

		id := uuid.Parse(host)
		if id == nil {
			// Host specified is IP or IP:port
			id, err = peer.GetPeerIDByAddrF(host)
			if err != nil {
				return nil, err
			}
		}

		nodes = append(nodes, id)
	}
	return nodes, nil
}

func volumeCreateHandler(w http.ResponseWriter, r *http.Request) {
	req := new(volume.VolCreateRequest)

	httpStatus, e := unmarshalVolCreateRequest(req, r)
	if e != nil {
		restutils.SendHTTPError(w, httpStatus, e.Error())
		return
	}

	if volume.ExistsFunc(req.Name) {
		restutils.SendHTTPError(w, http.StatusInternalServerError, gderrors.ErrVolExists.Error())
		return
	}

	reqid := uuid.NewRandom().String()
	logger := log.WithField("reqid", reqid)

	nodes, e := nodesForVolCreate(req)
	if e != nil {
		log.WithError(e).Error("could not prepare node list")
		restutils.SendHTTPError(w, http.StatusInternalServerError, e.Error())
		return
	}

	txn, e := (&transaction.SimpleTxn{
		Nodes:    nodes,
		LockKey:  req.Name,
		Stage:    "vol-create.Stage",
		Commit:   "vol-create.Commit",
		Store:    "vol-create.Store",
		Rollback: "vol-create.Rollback",
		LogFields: &log.Fields{
			"reqid": reqid,
		},
	}).NewTxn()
	if e != nil {
		logger.WithError(e).Error("failed to create transaction")
		restutils.SendHTTPError(w, http.StatusInternalServerError, e.Error())
		return
	}
	defer txn.Cleanup()

	e = txn.Ctx.Set("req", req)
	if e != nil {
		logger.WithError(e).Error("failed to set request in transaction context")
		restutils.SendHTTPError(w, http.StatusInternalServerError, e.Error())
		return
	}

	vol, e := createVolinfo(req)
	if e != nil {
		logger.WithError(e).Error("failed to create volinfo")
		restutils.SendHTTPError(w, http.StatusInternalServerError, e.Error())
		return
	}

	e = txn.Ctx.Set("volinfo", vol)
	if e != nil {
		logger.WithError(e).Error("failed to set volinfo in transaction context")
		restutils.SendHTTPError(w, http.StatusInternalServerError, e.Error())
		return
	}

	// Generate trusted username and password
	volAuth := volume.VolAuth{
		Username: uuid.NewRandom().String(),
		Password: uuid.NewRandom().String(),
	}
	e = txn.Ctx.Set("volauth", volAuth)
	if e != nil {
		logger.WithError(e).Error("failed to set trusted credentials in transaction context")
		restutils.SendHTTPError(w, http.StatusInternalServerError, e.Error())
		return
	}

	c, e := txn.Do()
	if e != nil {
		logger.WithError(e).Error("volume create transaction failed")
		restutils.SendHTTPError(w, http.StatusInternalServerError, e.Error())
		return
	}

	e = c.Get("volinfo", &vol)
	if e == nil {
		restutils.SendHTTPResponse(w, http.StatusCreated, vol)
		c.Logger().WithField("volname", vol.Name).Info("new volume created")
	} else {
		restutils.SendHTTPError(w, http.StatusInternalServerError, "failed to get volinfo")
	}

	return
}
