package cloud

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	"k8s.io-bkp/kubernetes/staging/src/k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	// AI
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

type CloudProvider interface {
	CreateDisk(diskOptions *DiskOptions) (volumeID VolumeID, err error)
	DeleteDisk(volumeID VolumeID) (bool, error)
	GetVolumesByTagName(tagKey, tagVal string) ([]string, error)
}

type DiskOptions struct {
	CapacityGB int
	Tags       map[string]string
	VolumeType string
	IOPSPerGB  int
}

type awsEBS struct {
	ec2     *ec2.EC2
	tagging awsTagging
}

func NewCloudProvider() (*awsEBS, error) {
	cfg, err := readAWSCloudConfig(nil)
	if err != nil {
		return nil, fmt.Errorf("unable to read AWS config file: %v", err)
	}

	sess, err := session.NewSession(&aws.Config{})
	if err != nil {
		return nil, fmt.Errorf("unable to initialize AWS session: %v", err)
	}

	var provider credentials.Provider
	if cfg.Global.RoleARN == "" {
		provider = &ec2rolecreds.EC2RoleProvider{
			Client: ec2metadata.New(sess),
		}
	} else {
		glog.Infof("Using AWS assumed role %v", cfg.Global.RoleARN)
		provider = &stscreds.AssumeRoleProvider{
			Client:  sts.New(sess),
			RoleARN: cfg.Global.RoleARN,
		}
	}

	creds := credentials.NewChainCredentials(
		[]credentials.Provider{
			&credentials.EnvProvider{},
			provider,
			&credentials.SharedCredentialsProvider{},
		})

	regionName := "us-east-1"
	awsConfig := &aws.Config{
		Region:      &regionName,
		Credentials: creds,
	}
	awsConfig = awsConfig.WithCredentialsChainVerboseErrors(true)

	return &awsEBS{
		ec2: ec2.New(session.New(awsConfig)),
	}, nil
}

func (c *awsEBS) CreateDisk(diskOptions *DiskOptions) (VolumeID, error) {
	var createType string
	var iops int64
	switch diskOptions.VolumeType {
	case VolumeTypeGP2, VolumeTypeSC1, VolumeTypeST1:
		createType = diskOptions.VolumeType

	case VolumeTypeIO1:
		createType = diskOptions.VolumeType
		iops = int64(diskOptions.CapacityGB * diskOptions.IOPSPerGB)
		if iops < MinTotalIOPS {
			iops = MinTotalIOPS
		}
		if iops > MaxTotalIOPS {
			iops = MaxTotalIOPS
		}

	case "":
		createType = DefaultVolumeType

	default:
		return "", fmt.Errorf("invalid AWS VolumeType %q", diskOptions.VolumeType)
	}

	request := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1d"), // TODO: read this from config file
		Size:             aws.Int64(int64(diskOptions.CapacityGB)),
		VolumeType:       aws.String(createType),
	}
	if iops > 0 {
		request.Iops = aws.Int64(iops)
	}

	response, err := c.ec2.CreateVolume(request)
	if err != nil {
		return "", err
	}

	awsID := awsVolumeID(aws.StringValue(response.VolumeId))
	if awsID == "" {
		return "", fmt.Errorf("VolumeID was not returned by CreateVolume")
	}
	volumeID := VolumeID("aws://" + aws.StringValue(response.AvailabilityZone) + "/" + string(awsID))

	if err := c.tagging.createTags(c.ec2, string(awsID), ResourceLifecycleOwned, diskOptions.Tags); err != nil {
		// delete the volume and hope it succeeds
		_, delerr := c.DeleteDisk(volumeID)
		if delerr != nil {
			// delete did not succeed, we have a stray volume!
			return "", fmt.Errorf("error tagging volume %s, could not delete the volume: %q", volumeID, delerr)
		}
		return "", fmt.Errorf("error tagging volume %s: %q", volumeID, err)
	}

	return volumeID, nil
}

func (c *awsEBS) DeleteDisk(volumeID VolumeID) (bool, error) {
	awsVolID, err := volumeID.MapToAWSVolumeID()
	if err != nil {
		return false, err
	}

	request := &ec2.DeleteVolumeInput{VolumeId: awsVolID.awsString()}
	_, err = c.ec2.DeleteVolume(request)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (c *awsEBS) AttachDisk(diskName VolumeID, nodeName types.NodeName) (string, error) {
	return "", nil
}

func (c *awsEBS) DetachDisk(diskName VolumeID, nodeName types.NodeName) (string, error) {
	return "", nil
}

func (c *awsEBS) GetVolumeLabels(volumeID VolumeID) (map[string]string, error) {
	return nil, nil
}

func (c *awsEBS) GetDiskPath(volumeID VolumeID) (string, error) {
	return "", nil
}

func (c *awsEBS) DiskIsAttached(diskName VolumeID, nodeName types.NodeName) (bool, error) {
	return false, nil
}

func (c *awsEBS) DisksAreAttached(map[types.NodeName][]VolumeID) (map[types.NodeName]map[VolumeID]bool, error) {
	return nil, nil
}

func (c *awsEBS) ResizeDisk(diskName VolumeID, oldSize resource.Quantity, newSize resource.Quantity) (resource.Quantity, error) {
	return resource.Quantity{}, nil
}

func (c *awsEBS) GetVolumesByTagName(tagKey, tagVal string) ([]string, error) {
	var volumes []string
	var nextToken *string
	request := &ec2.DescribeVolumesInput{}
	for {
		response, err := c.ec2.DescribeVolumes(request)
		if err != nil {
			return nil, err
		}
		for _, volume := range response.Volumes {
			for _, tag := range volume.Tags {
				if *tag.Key == tagKey && *tag.Value == tagVal {
					volumes = append(volumes, *volume.VolumeId)
					break
				}
			}
		}
		nextToken = response.NextToken
		if aws.StringValue(nextToken) == "" {
			break
		}
		request.NextToken = nextToken
	}
	return volumes, nil
}