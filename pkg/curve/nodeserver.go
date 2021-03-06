/*
Copyright 2020 The Netease Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package curve

import (
	"context"
	"fmt"
	"os"
	"path"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	utilexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
	utilpath "k8s.io/utils/path"

	csicommon "github.com/opencurve/curve-csi/pkg/csi-common"
	"github.com/opencurve/curve-csi/pkg/curveservice"
	"github.com/opencurve/curve-csi/pkg/util"
)

type nodeServer struct {
	*csicommon.DefaultNodeServer
	mounter     mount.Interface
	volumeLocks *util.VolumeLocks
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if err := ns.validateNodeStageVolumeRequest(req); err != nil {
		return nil, err
	}

	volumeId := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()

	if acquired := ns.volumeLocks.TryAcquire(volumeId); !acquired {
		klog.Infof(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), volumeId)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volumeId)
	}
	defer ns.volumeLocks.Release(volumeId)

	// check if stagingPath is already mounted
	isNotMnt, err := mount.IsNotMountPoint(ns.mounter, stagingTargetPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !isNotMnt {
		klog.Infof(util.Log(ctx, "volume %s is already mounted to %s, skipping"), volumeId, stagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// attach
	devicePath, err := ns.attachDevice(ctx, req)
	if err != nil {
		return nil, err
	}

	// create targetPath
	isBlock := req.GetVolumeCapability().GetBlock() != nil
	err = ns.createStageMountPoint(ctx, stagingTargetPath, isBlock)
	if err != nil {
		return nil, err
	}

	// nodeStage Path
	err = ns.mountVolumeToStagePath(ctx, req, stagingTargetPath, devicePath)
	if err != nil {
		return nil, err
	}

	// #nosec - allow anyone to write inside the target path
	err = os.Chmod(stagingTargetPath, 0777)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.Infof(util.Log(ctx, "successfully mounted volume %s to stagingTargetPath %s"), req.GetVolumeId(), stagingTargetPath)
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) attachDevice(ctx context.Context, req *csi.NodeStageVolumeRequest) (string, error) {
	isBlock := req.GetVolumeCapability().GetBlock() != nil
	disableInUseCheck := false
	// MULTI_NODE_MULTI_WRITER is supported by default for Block access type volumes
	if req.VolumeCapability.AccessMode.Mode == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER {
		if isBlock {
			disableInUseCheck = true
		} else {
			klog.Warningf(util.Log(ctx, "MULTI_NODE_MULTI_WRITER currently only supported with volumes of access type `block`, invalid AccessMode for volume: %v"), req.GetVolumeId())
			return "", status.Error(codes.InvalidArgument, "rbd: RWX access mode request is only valid for volumes with access type `block`")
		}
	}

	volOptions, err := newVolumeOptionsFromVolID(req.GetVolumeId())
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	klog.V(5).Infof(util.Log(ctx, "get volume options: %+v"), volOptions)

	curveVol := curveservice.NewCurveVolume(volOptions.user, volOptions.volName, volOptions.sizeGiB)
	devicePath, err := curveVol.Map(ctx, disableInUseCheck)
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	klog.Infof(util.Log(ctx, "curve file %s successfully mapped at %s"), curveVol.FilePath, devicePath)
	return devicePath, nil
}

func (ns *nodeServer) createStageMountPoint(ctx context.Context, mountPath string, isBlock bool) error {
	dirPath := path.Dir(mountPath)
	exists, err := utilpath.Exists(utilpath.CheckFollowSymlink, dirPath)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if !exists {
		if err := os.Mkdir(dirPath, 0750); err != nil {
			klog.Errorf(util.Log(ctx, "failed to create dir %s, err: %v"), dirPath, err)
			return status.Error(codes.Internal, err.Error())
		}
	}

	if isBlock {
		pathFile, err := os.OpenFile(mountPath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			klog.Errorf(util.Log(ctx, "failed to create mountPath:%s with error: %v"), mountPath, err)
			return status.Error(codes.Internal, err.Error())
		}
		if err = pathFile.Close(); err != nil {
			klog.Errorf(util.Log(ctx, "failed to close mountPath:%s with error: %v"), mountPath, err)
			return status.Error(codes.Internal, err.Error())
		}
		return nil
	}

	exists, err = utilpath.Exists(utilpath.CheckFollowSymlink, mountPath)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if !exists {
		if err := os.Mkdir(mountPath, 0750); err != nil {
			if !os.IsExist(err) {
				klog.Errorf(util.Log(ctx, "failed to create mountPath: %s with err: %v"), mountPath, err)
				return status.Error(codes.Internal, err.Error())
			}
		}
	}

	return nil
}

func (ns *nodeServer) mountVolumeToStagePath(ctx context.Context, req *csi.NodeStageVolumeRequest, stagingPath, devicePath string) error {
	fsType := req.GetVolumeCapability().GetMount().GetFsType()
	diskMounter := &mount.SafeFormatAndMount{Interface: ns.mounter, Exec: utilexec.New()}

	var err error
	opt := []string{"_netdev"}
	opt = csicommon.ConstructMountOptions(opt, req.GetVolumeCapability())
	if req.GetVolumeCapability().GetBlock() != nil {
		opt = append(opt, "bind")
		err = diskMounter.Mount(devicePath, stagingPath, fsType, opt)
	} else {
		err = diskMounter.FormatAndMount(devicePath, stagingPath, fsType, opt)
	}
	if err != nil {
		klog.Errorf(util.Log(ctx, "failed to mount device path (%s) to staging path (%s) for volume (%s) error %s"), devicePath, stagingPath, req.GetVolumeId(), err)
		return status.Error(codes.Internal, err.Error())
	}

	return nil
}

// NodePublishVolume mounts the volume mounted to the device path to the target path
func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if err := ns.validateNodePublishVolumeRequest(req); err != nil {
		return nil, err
	}

	volumeId := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	stagingPath := req.GetStagingTargetPath()
	fsType := req.GetVolumeCapability().GetMount().GetFsType()
	isBlock := req.GetVolumeCapability().GetBlock() != nil

	if acquired := ns.volumeLocks.TryAcquire(volumeId); !acquired {
		klog.Infof(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), volumeId)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volumeId)
	}
	defer ns.volumeLocks.Release(volumeId)

	// Check if that target path exists properly
	notMnt, err := ns.createTargetMountPath(ctx, targetPath, isBlock)
	if err != nil {
		return nil, err
	}
	if !notMnt {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// Publish Path
	mountOptions := []string{"bind", "_netdev"}
	mountOptions = csicommon.ConstructMountOptions(mountOptions, req.GetVolumeCapability())
	klog.V(4).Infof(util.Log(ctx, "target %v\nisBlock %v\nfstype %v\nstagingPath %v\nreadonly %v\nmountflags %v\n"),
		targetPath, isBlock, fsType, stagingPath, req.GetReadonly(), mountOptions)
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}
	if err := mount.New("").Mount(stagingPath, targetPath, fsType, mountOptions); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.Infof(util.Log(ctx, "successfully mounted stagingPath %s to targetPath %s"), stagingPath, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) createTargetMountPath(ctx context.Context, mountPath string, isBlock bool) (bool, error) {
	// Check if that mount path exists properly
	notMnt, err := mount.IsNotMountPoint(ns.mounter, mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			if isBlock {
				// #nosec
				pathFile, e := os.OpenFile(mountPath, os.O_CREATE|os.O_RDWR, 0750)
				if e != nil {
					klog.V(4).Infof(util.Log(ctx, "Failed to create mountPath:%s with error: %v"), mountPath, err)
					return notMnt, status.Error(codes.Internal, e.Error())
				}
				if err = pathFile.Close(); err != nil {
					klog.V(4).Infof(util.Log(ctx, "Failed to close mountPath:%s with error: %v"), mountPath, err)
					return notMnt, status.Error(codes.Internal, err.Error())
				}
			} else {
				// Create a directory
				if err = os.MkdirAll(mountPath, 0750); err != nil {
					return notMnt, status.Error(codes.Internal, err.Error())
				}
			}
			notMnt = true
		} else {
			return false, status.Error(codes.Internal, err.Error())
		}
	}
	return notMnt, err
}

// NodeUnpublishVolume unmounts the volume from the target path
func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if err := ns.validateNodeUnpublishVolumeRequest(req); err != nil {
		return nil, err
	}

	volumeId := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	if acquired := ns.volumeLocks.TryAcquire(volumeId); !acquired {
		klog.Infof(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), volumeId)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volumeId)
	}
	defer ns.volumeLocks.Release(volumeId)

	notMnt, err := mount.IsNotMountPoint(ns.mounter, targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			// targetPath has already been deleted
			klog.V(4).Infof(util.Log(ctx, "targetPath: %s has already been deleted"), targetPath)
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if notMnt {
		if err = os.RemoveAll(targetPath); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err = ns.mounter.Unmount(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err = os.RemoveAll(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.Infof(util.Log(ctx, "rbd: successfully unbound volume %s from %s"), volumeId, targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeUnstageVolume unstages the volume from the staging path
func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if err := ns.validateNodeUnstageVolumeRequest(req); err != nil {
		return nil, err
	}

	volumeId := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()

	if acquired := ns.volumeLocks.TryAcquire(volumeId); !acquired {
		klog.Infof(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), volumeId)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volumeId)
	}
	defer ns.volumeLocks.Release(volumeId)

	notMnt, err := mount.IsNotMountPoint(ns.mounter, stagingTargetPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		// Continue on ENOENT errors as we may still have the image mapped
		notMnt = true
	}
	if !notMnt {
		// Unmounting the targetPath
		err = ns.mounter.Unmount(stagingTargetPath)
		if err != nil {
			klog.V(3).Infof(util.Log(ctx, "failed to unmount targetPath: %s with err: %v"), stagingTargetPath, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if err = os.Remove(stagingTargetPath); err != nil {
		if !os.IsNotExist(err) {
			klog.Errorf(util.Log(ctx, "failed to remove staging target path: %s with err: (%v)"), stagingTargetPath, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	// unmap
	volOptions, err := newVolumeOptionsFromVolID(volumeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	klog.V(5).Infof(util.Log(ctx, "get volume options: %+v"), volOptions)
	curveVol := curveservice.NewCurveVolume(volOptions.user, volOptions.volName, volOptions.sizeGiB)
	if err := curveVol.UnMap(ctx); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.Infof(util.Log(ctx, "successfully unmounted volume %s from stagingPath %s"), volumeId, stagingTargetPath)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodeExpandVolume expands the volume
func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if err := ns.validateNodeExpandVolumeRequest(req); err != nil {
		return nil, err
	}

	volumeId := req.GetVolumeId()
	volumePath := req.GetVolumePath()

	if acquired := ns.volumeLocks.TryAcquire(volumeId); !acquired {
		klog.Infof(util.Log(ctx, util.VolumeOperationAlreadyExistsFmt), volumeId)
		return nil, status.Errorf(codes.Aborted, util.VolumeOperationAlreadyExistsFmt, volumeId)
	}
	defer ns.volumeLocks.Release(volumeId)

	// get device path
	devicePath, _, err := mount.GetDeviceNameFromMount(ns.mounter, volumePath)
	if err != nil {
		return nil, fmt.Errorf("can not get device from mount, err: %v", err)
	}
	if devicePath == "" {
		klog.V(4).Infof("the path %s is not mounted, ignore resizing", volumePath)
		return &csi.NodeExpandVolumeResponse{}, nil
	}

	diskMounter := &mount.SafeFormatAndMount{Interface: ns.mounter, Exec: utilexec.New()}
	// TODO check size and return success or error
	resizer := util.NewResizeFs(diskMounter)
	ok, err := resizer.Resize(devicePath, volumePath)
	if !ok {
		return nil, fmt.Errorf("resize failed on path %s, error: %v", volumePath, err)
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

// NodeGetVolumeStats returns volume stats
func (ns *nodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Errorf(codes.InvalidArgument, "volumePath %v is empty", volumePath)
	}

	exists, err := utilpath.Exists(utilpath.CheckFollowSymlink, volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check whether volumePath exists: %s", err)
	}
	if !exists {
		return nil, status.Errorf(codes.NotFound, "target: %s not found", volumePath)
	}

	stats, err := util.GetDeviceStats(volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get stats by path: %s", err)
	}

	klog.V(5).Infof(util.Log(ctx, "get volumePath %q stats: %+v"), volumePath, stats)

	if stats.Block {
		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				{
					Total: stats.TotalBytes,
					Unit:  csi.VolumeUsage_BYTES,
				},
			},
		}, nil
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Total:     stats.TotalBytes,
				Available: stats.AvailableBytes,
				Used:      stats.UsedBytes,
				Unit:      csi.VolumeUsage_BYTES,
			}, {
				Total:     stats.TotalInodes,
				Available: stats.AvailableInodes,
				Used:      stats.UsedInodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}

// NodeGetCapabilities returns the supported capabilities of the node server
func (ns *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			}, {
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
		},
	}, nil
}
