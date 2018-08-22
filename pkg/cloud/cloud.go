/*
Copyright 2018 The Kubernetes Authors.

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

package cloud

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/bertinatto/ebs-csi-driver/pkg/util"
	"github.com/golang/glog"
)

const (
	// TODO: what should be the default size?
	// DefaultVolumeSize represents the default volume size.
	DefaultVolumeSize int64 = 1 * 1024 * 1024 * 1024

	// VolumeNameTagKey is the key value that refers to the volume's name.
	VolumeNameTagKey = "com.amazon.aws.csi.volume"

	// VolumeTypeIO1 represents a provisioned IOPS SSD type of volume.
	VolumeTypeIO1 = "io1"

	// VolumeTypeGP2 represents a general purpose SSD type of volume.
	VolumeTypeGP2 = "gp2"

	// VolumeTypeSC1 represents a cold HDD (sc1) type of volume.
	VolumeTypeSC1 = "sc1"

	// VolumeTypeST1 represents a throughput-optimized HDD type of volume.
	VolumeTypeST1 = "st1"

	// MinTotalIOPS represents the minimum Input Output per second.
	MinTotalIOPS int64 = 100

	// MaxTotalIOPS represents the maximum Input Output per second.
	MaxTotalIOPS int64 = 20000

	// DefaultVolumeType specifies which storage to use for newly created Volumes.
	DefaultVolumeType = VolumeTypeGP2
)

var (
	// ErrMultiDisks is an error that is returned when multiple
	// disks are found with the same volume name.
	ErrMultiDisks = errors.New("Multiple disks with same name")

	// ErrDiskExistsDiffSize is an error that is returned if a disk with a given
	// name, but different size, is found.
	ErrDiskExistsDiffSize = errors.New("There is already a disk with same name and different size")

	// ErrVolumeNotFound is returned when a volume with a given ID is not found.
	ErrVolumeNotFound = errors.New("Volume was not found")
)

type Disk struct {
	VolumeID    string
	CapacityGiB int64
}

type DiskOptions struct {
	CapacityBytes int64
	Tags          map[string]string
	VolumeType    string
	IOPSPerGB     int64
}

// EC2 abstracts aws.EC2 to facilitate its mocking.
type EC2 interface {
	DescribeVolumes(input *ec2.DescribeVolumesInput) (*ec2.DescribeVolumesOutput, error)
	CreateVolume(input *ec2.CreateVolumeInput) (*ec2.Volume, error)
	DeleteVolume(input *ec2.DeleteVolumeInput) (*ec2.DeleteVolumeOutput, error)
	DetachVolume(input *ec2.DetachVolumeInput) (*ec2.VolumeAttachment, error)
	AttachVolume(input *ec2.AttachVolumeInput) (*ec2.VolumeAttachment, error)
	DescribeInstances(input *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error)
}

type Compute interface {
	GetMetadata() *Metadata
	CreateDisk(string, *DiskOptions) (*Disk, error)
	DeleteDisk(string) (bool, error)
	AttachDisk(string, string) (string, error)
	DetachDisk(string, string) error
	GetDiskByNameAndSize(string, int64) (*Disk, error)
}

type Cloud struct {
	metadata *Metadata
	dm       DeviceManager

	ec2 EC2
}

var _ Compute = &Cloud{}

func NewCloud() (*Cloud, error) {
	sess, err := session.NewSession(&aws.Config{})
	if err != nil {
		return nil, fmt.Errorf("unable to initialize AWS session: %v", err)
	}

	svc := ec2metadata.New(sess)

	metadata, err := NewMetadata(svc)
	if err != nil {
		return nil, fmt.Errorf("could not get metadata from AWS: %v", err)
	}

	provider := []credentials.Provider{
		&credentials.EnvProvider{},
		&ec2rolecreds.EC2RoleProvider{Client: svc},
		&credentials.SharedCredentialsProvider{},
	}

	awsConfig := &aws.Config{
		Region:      aws.String(metadata.GetRegion()),
		Credentials: credentials.NewChainCredentials(provider),
	}
	awsConfig = awsConfig.WithCredentialsChainVerboseErrors(true)

	cloud := &Cloud{
		metadata: metadata,
		ec2:      ec2.New(session.New(awsConfig)),
	}
	cloud.dm = NewDeviceManager(cloud)

	return cloud, nil
}

func (c *Cloud) GetMetadata() *Metadata {
	return c.metadata
}

func (c *Cloud) CreateDisk(volumeName string, diskOptions *DiskOptions) (*Disk, error) {
	var createType string
	var iops int64
	capacityGiB := util.BytesToGiB(diskOptions.CapacityBytes)

	switch diskOptions.VolumeType {
	case VolumeTypeGP2, VolumeTypeSC1, VolumeTypeST1:
		createType = diskOptions.VolumeType
	case VolumeTypeIO1:
		createType = diskOptions.VolumeType
		iops = capacityGiB * diskOptions.IOPSPerGB
		if iops < MinTotalIOPS {
			iops = MinTotalIOPS
		}
		if iops > MaxTotalIOPS {
			iops = MaxTotalIOPS
		}
	case "":
		createType = DefaultVolumeType
	default:
		return nil, fmt.Errorf("invalid AWS VolumeType %q", diskOptions.VolumeType)
	}

	var tags []*ec2.Tag
	for key, value := range diskOptions.Tags {
		tags = append(tags, &ec2.Tag{Key: &key, Value: &value})
	}
	tagSpec := ec2.TagSpecification{
		ResourceType: aws.String("volume"),
		Tags:         tags,
	}

	m := c.GetMetadata()
	request := &ec2.CreateVolumeInput{
		AvailabilityZone:  aws.String(m.GetAvailabilityZone()),
		Size:              aws.Int64(capacityGiB),
		VolumeType:        aws.String(createType),
		TagSpecifications: []*ec2.TagSpecification{&tagSpec},
	}
	if iops > 0 {
		request.Iops = aws.Int64(iops)
	}

	response, err := c.ec2.CreateVolume(request)
	if err != nil {
		return nil, fmt.Errorf("could not create volume in EC2: %v", err)
	}

	volumeID := aws.StringValue(response.VolumeId)
	if len(volumeID) == 0 {
		return nil, fmt.Errorf("volume ID was not returned by CreateVolume")
	}

	size := aws.Int64Value(response.Size)
	if size == 0 {
		return nil, fmt.Errorf("disk size was not returned by CreateVolume")
	}

	return &Disk{CapacityGiB: size, VolumeID: volumeID}, nil
}

func (c *Cloud) DeleteDisk(volumeID string) (bool, error) {
	request := &ec2.DeleteVolumeInput{VolumeId: &volumeID}
	if _, err := c.ec2.DeleteVolume(request); err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == "InvalidVolume.NotFound" {
				return false, ErrVolumeNotFound
			}
		}
		return false, fmt.Errorf("DeleteDisk could not delete volume: %v", err)
	}
	return true, nil
}

func (c *Cloud) AttachDisk(volumeID, nodeID string) (string, error) {
	mntDevice, alreadyAttached, mntErr := c.dm.GetDevice(nodeID, volumeID)
	if mntErr != nil {
		return "", mntErr
	}

	// attachEnded is set to true if the attach operation completed
	// (successfully or not), and is thus no longer in progress
	attachEnded := false
	defer func() {
		if attachEnded {
			c.dm.ReleaseDevice(nodeID, volumeID, mntDevice)
		}
	}()

	device := mntDevice
	if !alreadyAttached {
		request := &ec2.AttachVolumeInput{
			Device:     aws.String(device),
			InstanceId: aws.String(nodeID),
			VolumeId:   aws.String(volumeID),
		}

		resp, err := c.ec2.AttachVolume(request)
		if err != nil {
			attachEnded = true
			return "", fmt.Errorf("could not attach volume %q to node %q: %v", volumeID, nodeID, err)
		}
		glog.V(2).Infof("AttachVolume volume=%q instance=%q request returned %v", volumeID, nodeID, resp)
	}

	// TODO: wait attaching
	//attachment, err := disk.waitForAttachmentStatus("attached")

	//if err != nil {
	//if err == wait.ErrWaitTimeout {
	//c.applyUnSchedulableTaint(nodeName, "Volume stuck in attaching state - node needs reboot to fix impaired state.")
	//}
	//return "", err
	//}

	// The attach operation has finished
	attachEnded = true

	// Double check the attachment to be 100% sure we attached the correct volume at the correct mountpoint
	// It could happen otherwise that we see the volume attached from a previous/separate AttachVolume call,
	// which could theoretically be against a different device (or even instance).
	//if attachment == nil {
	//// Impossible?
	//return "", fmt.Errorf("unexpected state: attachment nil after attached %q to %q", diskName, nodeName)
	//}
	//if ec2Device != aws.StringValue(attachment.Device) {
	//return "", fmt.Errorf("disk attachment of %q to %q failed: requested device %q but found %q", diskName, nodeName, ec2Device, aws.StringValue(attachment.Device))
	//}
	//if awsInstance.awsID != aws.StringValue(attachment.InstanceId) {
	//return "", fmt.Errorf("disk attachment of %q to %q failed: requested instance %q but found %q", diskName, nodeName, awsInstance.awsID, aws.StringValue(attachment.InstanceId))
	//}
	//return hostDevice, nil

	return device, nil
}

func (c *Cloud) DetachDisk(volumeID, nodeID string) error {
	// TODO: check if attached
	mntDevice, err := c.dm.GetAssignedDevice(nodeID, volumeID)
	if err != nil {
		return err
	}

	request := &ec2.DetachVolumeInput{
		InstanceId: aws.String(nodeID),
		VolumeId:   aws.String(volumeID),
	}

	_, err = c.ec2.DetachVolume(request)
	if err != nil {
		return fmt.Errorf("could not detach volume %q from node %q: %v", volumeID, nodeID, err)
	}

	//c.dm.DeprioritizeDevice(nodeID, mntDevice)

	if mntDevice != "" {
		c.dm.ReleaseDevice(nodeID, volumeID, mntDevice)
		// We don't check the return value - we don't really expect the attachment to have been
		// in progress, though it might have been
	}

	return nil
}

func (c *Cloud) GetDiskByNameAndSize(name string, capacityBytes int64) (*Disk, error) {
	var volumes []*ec2.Volume
	var nextToken *string

	request := &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   aws.String("tag:" + VolumeNameTagKey),
				Values: []*string{aws.String(name)},
			},
		},
	}
	for {
		response, err := c.ec2.DescribeVolumes(request)
		if err != nil {
			return nil, err
		}
		for _, volume := range response.Volumes {
			volumes = append(volumes, volume)
		}
		nextToken = response.NextToken
		if aws.StringValue(nextToken) == "" {
			break
		}
		request.NextToken = nextToken
	}

	if len(volumes) > 1 {
		return nil, ErrMultiDisks
	}

	if len(volumes) == 0 {
		return nil, nil
	}

	volSizeBytes := aws.Int64Value(volumes[0].Size)
	if volSizeBytes != util.BytesToGiB(capacityBytes) {
		return nil, ErrDiskExistsDiffSize
	}

	return &Disk{
		VolumeID:    aws.StringValue(volumes[0].VolumeId),
		CapacityGiB: volSizeBytes,
	}, nil
}

func (c *Cloud) getInstance(nodeID string) (*ec2.Instance, error) {
	results := []*ec2.Instance{}
	request := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{&nodeID},
	}

	var nextToken *string
	for {
		response, err := c.ec2.DescribeInstances(request)
		if err != nil {
			return nil, fmt.Errorf("error listing AWS instances: %q", err)
		}

		for _, reservation := range response.Reservations {
			results = append(results, reservation.Instances...)
		}

		nextToken = response.NextToken
		if aws.StringValue(nextToken) == "" {
			break
		}
		request.NextToken = nextToken
	}

	nInstances := len(results)
	if nInstances != 1 {
		return nil, fmt.Errorf("expected 1 instance with ID %q, got %d", nodeID, len(results))
	}

	instance := results[0]
	return instance, nil
}
