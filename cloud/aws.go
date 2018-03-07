package cloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

const (
	defaultAWSRegion = "us-west-2"
	gbDivider        = 1024.0 * 1024.0 * 1024.0
	awsStateInUse    = "in-use"
)

// awsResourceManager uses the AWS Go SDK. Docs can be found at:
// https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/
type awsResourceManager struct {
	accounts []string
}

func (m *awsResourceManager) Owners() []string {
	return m.accounts
}

const (
	assumeRoleARNTemplate = "arn:aws:iam::%s:role/brkt-HouseKeeper"

	accessDeniedErrorCode = "AccessDenied"
	unauthorizedErrorCode = "UnauthorizedOperation"
	notFoundErrorOcde     = "NotFound"

	snapshotIDFilterName = "block-device-mapping.snapshot-id"
)

var (
	instanceStateFilterName = "instance-state-name"
	instanceStateRunning    = ec2.InstanceStateNameRunning

	awsOwnerIDSelfValue = "self"
)

func (m *awsResourceManager) InstancesPerAccount() map[string][]Instance {
	log.Println("Getting instances in all accounts")
	resultMap := make(map[string][]Instance)
	getAllEC2Resources(m.accounts, func(client *ec2.EC2, account string) {
		instances, err := getAWSInstances(account, client)
		if err != nil {
			handleAWSAccessDenied(account, err)
		} else if len(instances) > 0 {
			resultMap[account] = append(resultMap[account], instances...)
		}
	})
	return resultMap
}

func (m *awsResourceManager) ImagesPerAccount() map[string][]Image {
	log.Println("Getting images in all accounts")
	resultMap := make(map[string][]Image)
	getAllEC2Resources(m.accounts, func(client *ec2.EC2, account string) {
		images, err := getAWSImages(account, client)
		if err != nil {
			handleAWSAccessDenied(account, err)
		} else if len(images) > 0 {
			resultMap[account] = append(resultMap[account], images...)
		}
	})
	return resultMap
}

func (m *awsResourceManager) VolumesPerAccount() map[string][]Volume {
	log.Println("Getting volumes in all accounts")
	resultMap := make(map[string][]Volume)
	getAllEC2Resources(m.accounts, func(client *ec2.EC2, account string) {
		volumes, err := getAWSVolumes(account, client)
		if err != nil {
			handleAWSAccessDenied(account, err)
		} else if len(volumes) > 0 {
			resultMap[account] = append(resultMap[account], volumes...)
		}
	})
	return resultMap
}

func (m *awsResourceManager) SnapshotsPerAccount() map[string][]Snapshot {
	log.Println("Getting snapshots in all accounts")
	resultMap := make(map[string][]Snapshot)
	getAllEC2Resources(m.accounts, func(client *ec2.EC2, account string) {
		snapshots, err := getAWSSnapshots(account, client)
		if err != nil {
			handleAWSAccessDenied(account, err)
		} else if len(snapshots) > 0 {
			resultMap[account] = append(resultMap[account], snapshots...)
		}
	})
	return resultMap
}

func (m *awsResourceManager) AllResourcesPerAccount() map[string]*ResourceCollection {
	log.Println("Getting all resources in all accounts")
	resultMap := make(map[string]*ResourceCollection)
	for i := range m.accounts {
		resultMap[m.accounts[i]] = new(ResourceCollection)
	}
	// TODO: Smarter error handling. If one request get access denied, then might as
	// well abort. The rest are going to fail too.
	getAllEC2Resources(m.accounts, func(client *ec2.EC2, account string) {
		result := resultMap[account]
		result.Owner = account
		var wg sync.WaitGroup
		wg.Add(4)
		go func() {
			snapshots, err := getAWSSnapshots(account, client)
			if err != nil {
				log.Printf("Snapshot error when getting all resources in %s", account)
				handleAWSAccessDenied(account, err)
			}
			result.Snapshots = append(result.Snapshots, snapshots...)
			wg.Done()
		}()
		go func() {
			instances, err := getAWSInstances(account, client)
			if err != nil {
				log.Printf("Instance error when getting all resources in %s", account)
				handleAWSAccessDenied(account, err)
			}
			result.Instances = append(result.Instances, instances...)
			wg.Done()
		}()
		go func() {
			images, err := getAWSImages(account, client)
			if err != nil {
				log.Printf("Image error when getting all resources in %s", account)
				handleAWSAccessDenied(account, err)
			}
			result.Images = append(result.Images, images...)
			wg.Done()
		}()
		go func() {
			volumes, err := getAWSVolumes(account, client)
			if err != nil {
				log.Printf("Volume error when getting all resources in %s", account)
				handleAWSAccessDenied(account, err)
			}
			result.Volumes = append(result.Volumes, volumes...)
			wg.Done()
		}()
		wg.Wait()
		resultMap[account] = result
	})
	return resultMap
}

func (m *awsResourceManager) BucketsPerAccount() map[string][]Bucket {
	log.Println("Getting all buckets in all accounts")
	sess := session.Must(session.NewSession())
	resultMap := make(map[string][]Bucket)
	forEachAccount(m.accounts, sess, func(account string, cred *credentials.Credentials) {
		s3Client := s3.New(sess, &aws.Config{
			Credentials: cred,
			Region:      aws.String(defaultAWSRegion),
		})
		awsBuckets, err := s3Client.ListBuckets(&s3.ListBucketsInput{})
		if err != nil {
			log.Printf("Bucket error when getting buckets in %s", account)
			handleAWSAccessDenied(account, err)
		} else if len(awsBuckets.Buckets) > 0 {
			bucketCount := len(awsBuckets.Buckets)
			buckChan := make(chan *awsBucket)
			for _, bu := range awsBuckets.Buckets {
				go func(bu *s3.Bucket, resChan chan *awsBucket) {
					region, err := s3manager.GetBucketRegion(context.Background(), sess, *bu.Name, defaultAWSRegion)
					if err != nil {
						bucketCount--
						log.Printf("Couldn't determine bucket region in %s for bucket %s", account, *bu.Name)
						handleAWSAccessDenied(account, err)
						buckChan <- nil
						return
					}
					bucketClient := s3.New(sess, &aws.Config{
						Credentials: cred,
						Region:      aws.String(region),
					})
					buTags, err := bucketClient.GetBucketTagging(&s3.GetBucketTaggingInput{
						Bucket: bu.Name,
					})
					tags := make(map[string]string)
					if err == nil {
						tags = convertAWSS3Tags(buTags.TagSet)
					}

					var count, size int64
					var lastMod time.Time

					err = bucketClient.ListObjectsV2Pages(&s3.ListObjectsV2Input{
						Bucket: bu.Name,
					}, func(output *s3.ListObjectsV2Output, lastPage bool) bool {
						for _, obj := range output.Contents {
							count++
							size += *obj.Size
							if (*obj.LastModified).After(lastMod) {
								lastMod = *obj.LastModified
							}
						}
						return !lastPage
					})
					if err != nil {
						bucketCount--
						log.Printf("Failed to list contents in bucket %s, account %s", *bu.Name, account)
						handleAWSAccessDenied(account, err)
						buckChan <- nil
						return
					}

					buck := awsBucket{baseBucket{
						baseResource: baseResource{
							csp:          AWS,
							owner:        account,
							location:     region,
							id:           *bu.Name,
							creationTime: *bu.CreationDate,
							tags:         tags,
						},
						lastModified: lastMod,
						objectCount:  count,
						totalSizeGB:  float64(size) / gbDivider,
					}}
					buckChan <- &buck
				}(bu, buckChan)
			}
			for i := 0; i < bucketCount; i++ {
				buck := <-buckChan
				if buck != nil {
					resultMap[account] = append(resultMap[account], buck)
				}
			}
		}
	})
	return resultMap
}

func (m *awsResourceManager) CleanupInstances(instances []Instance) error {
	resList := []Resource{}
	for i := range instances {
		v, ok := instances[i].(Resource)
		if !ok {
			return errors.New("Could not convert Instance to Resource")
		}
		resList = append(resList, v)
	}
	return cleanupResources(resList)
}

func (m *awsResourceManager) CleanupImages(images []Image) error {
	resList := []Resource{}
	for i := range images {
		v, ok := images[i].(Resource)
		if !ok {
			return errors.New("Could not convert Image to Resource")
		}
		resList = append(resList, v)
	}
	return cleanupResources(resList)
}

func (m *awsResourceManager) CleanupVolumes(volumes []Volume) error {
	resList := []Resource{}
	for i := range volumes {
		v, ok := volumes[i].(Resource)
		if !ok {
			return errors.New("Could not convert Volume to Resource")
		}
		resList = append(resList, v)
	}
	return cleanupResources(resList)
}

func (m *awsResourceManager) CleanupSnapshots(snapshots []Snapshot) error {
	resList := []Resource{}
	for i := range snapshots {
		v, ok := snapshots[i].(Resource)
		if !ok {
			return errors.New("Could not convert Snapshot to Resource")
		}
		resList = append(resList, v)
	}
	return cleanupResources(resList)
}

func (m *awsResourceManager) CleanupBuckets(buckets []Bucket) error {
	resList := []Resource{}
	for i := range buckets {
		v, ok := buckets[i].(Resource)
		if !ok {
			return errors.New("Could not convert Bucket to Resource")
		}
		resList = append(resList, v)
	}
	return cleanupResources(resList)
}

func cleanupResources(resources []Resource) error {
	failed := false
	var wg sync.WaitGroup
	wg.Add(len(resources))
	for i := range resources {
		go func(index int) {
			err := resources[index].Cleanup()
			if err != nil {
				log.Printf("Cleaning up %s for owner %s failed\n%s\n", resources[index].ID(), resources[index].Owner(), err)
				failed = true
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	if failed {
		return errors.New("One or more resource cleanups failed")
	}
	return nil
}

// getAWSInstances will get all running instances using an already
// set-up client for a specific credential and region.
func getAWSInstances(account string, client *ec2.EC2) ([]Instance, error) {
	// We're only interested in running instances
	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{&ec2.Filter{
			Name:   aws.String(instanceStateFilterName),
			Values: aws.StringSlice([]string{instanceStateRunning})}},
	}
	awsReservations, err := client.DescribeInstances(input)
	if err != nil {
		return nil, err
	}
	result := []Instance{}
	for _, reservation := range awsReservations.Reservations {
		for _, instance := range reservation.Instances {
			inst := awsInstance{baseInstance{
				baseResource: baseResource{
					csp:          AWS,
					owner:        account,
					id:           *instance.InstanceId,
					location:     *client.Config.Region,
					creationTime: *instance.LaunchTime,
					public:       instance.PublicIpAddress != nil,
					tags:         convertAWSTags(instance.Tags)},
				instanceType: *instance.InstanceType,
			}}
			result = append(result, &inst)
		}
	}
	return result, nil
}

// getAWSImages will get all AMIs owned by the current account
func getAWSImages(account string, client *ec2.EC2) ([]Image, error) {
	input := &ec2.DescribeImagesInput{
		Owners: aws.StringSlice([]string{awsOwnerIDSelfValue}),
	}
	awsImages, err := client.DescribeImages(input)
	if err != nil {
		return nil, err
	}
	result := []Image{}
	for _, ami := range awsImages.Images {
		ti, err := time.Parse(time.RFC3339, *ami.CreationDate)
		if err != nil {
			return nil, err
		}
		img := awsImage{baseImage{
			baseResource: baseResource{
				csp:          AWS,
				owner:        account,
				id:           *ami.ImageId,
				location:     *client.Config.Region,
				creationTime: ti,
				public:       *ami.Public,
				tags:         convertAWSTags(ami.Tags),
			},
			name: *ami.Name,
		}}
		for _, mapping := range ami.BlockDeviceMappings {
			if mapping != nil && (*mapping).Ebs != nil && (*(*mapping).Ebs).VolumeSize != nil {
				img.baseImage.sizeGB += *mapping.Ebs.VolumeSize
			}
		}
		result = append(result, &img)
	}
	return result, nil
}

// getAWSVolumes will get all volumes (both attached and un-attached)
// in the current account
func getAWSVolumes(account string, client *ec2.EC2) ([]Volume, error) {
	input := new(ec2.DescribeVolumesInput)
	awsVolumes, err := client.DescribeVolumes(input)
	if err != nil {
		return nil, err
	}
	result := []Volume{}
	for _, volume := range awsVolumes.Volumes {
		inUse := len(volume.Attachments) > 0 || *volume.State == awsStateInUse
		vol := awsVolume{baseVolume{
			baseResource: baseResource{
				csp:          AWS,
				owner:        account,
				id:           *volume.VolumeId,
				location:     *client.Config.Region,
				creationTime: *volume.CreateTime,
				public:       false,
				tags:         convertAWSTags(volume.Tags),
			},
			sizeGB:     *volume.Size,
			attached:   inUse,
			encrypted:  *volume.Encrypted,
			volumeType: *volume.VolumeType,
		}}
		result = append(result, &vol)
	}
	return result, nil
}

// getAWSSnapshots will get all snapshots in AWS owned
// by the current account
func getAWSSnapshots(account string, client *ec2.EC2) ([]Snapshot, error) {
	input := &ec2.DescribeSnapshotsInput{
		OwnerIds: aws.StringSlice([]string{awsOwnerIDSelfValue}),
	}
	awsSnapshots, err := client.DescribeSnapshots(input)
	if err != nil {
		return nil, err
	}
	result := []Snapshot{}
	snapshotsInUse := getSnapshotsInUse(client)
	for _, snapshot := range awsSnapshots.Snapshots {
		_, inUse := snapshotsInUse[*snapshot.SnapshotId]
		snap := awsSnapshot{baseSnapshot{
			baseResource: baseResource{
				csp:          AWS,
				owner:        account,
				id:           *snapshot.SnapshotId,
				location:     *client.Config.Region,
				creationTime: *snapshot.StartTime,
				public:       false,
				tags:         convertAWSTags(snapshot.Tags),
			},
			sizeGB:    *snapshot.VolumeSize,
			encrypted: *snapshot.Encrypted,
			inUse:     inUse,
		}}
		result = append(result, &snap)
	}
	return result, nil
}

func getSnapshotsInUse(client *ec2.EC2) map[string]struct{} {
	result := make(map[string]struct{})
	input := &ec2.DescribeImagesInput{
		Owners: aws.StringSlice([]string{awsOwnerIDSelfValue}),
	}
	images, err := client.DescribeImages(input)
	if err != nil {
		log.Printf("Could not determine snapshots in use:\n%s\n", err)
		return result
	}
	for _, imgs := range images.Images {
		for _, mapping := range imgs.BlockDeviceMappings {
			if mapping != nil && mapping.Ebs != nil && mapping.Ebs.SnapshotId != nil {
				result[*mapping.Ebs.SnapshotId] = struct{}{}
			}
		}
	}
	return result
}

func getAllEC2Resources(accounts []string, funcToRun func(client *ec2.EC2, account string)) {
	sess := session.Must(session.NewSession())
	forEachAccount(accounts, sess, func(account string, cred *credentials.Credentials) {
		log.Println("Accessing account", account)
		forEachAWSRegion(func(region string) {
			client := ec2.New(sess, &aws.Config{
				Credentials: cred,
				Region:      aws.String(region),
			})
			funcToRun(client, account)
		})
	})
}

// forEachAccount is a higher order function that will, for
// every account, create credentials and call the specified
// function with those creds
func forEachAccount(accounts []string, sess *session.Session, funcToRun func(account string, cred *credentials.Credentials)) {
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func(x int) {
			creds := stscreds.NewCredentials(sess, fmt.Sprintf(assumeRoleARNTemplate, accounts[x]))
			funcToRun(accounts[x], creds)
			wg.Done()
		}(i)
	}
	wg.Wait()
}

// forEachAWSRegion is a higher order function that will, for
// every available AWS region, run the specified function
func forEachAWSRegion(funcToRun func(region string)) {
	regions, exists := endpoints.RegionsForService(endpoints.DefaultPartitions(), endpoints.AwsPartitionID, endpoints.Ec2ServiceID)
	if !exists {
		panic("The regions for EC2 in the standard partition should exist")
	}
	var wg sync.WaitGroup
	for regionID := range regions {
		wg.Add(1)
		go func(x string) {
			funcToRun(x)
			wg.Done()
		}(regionID)
	}
	wg.Wait()
}

func handleAWSAccessDenied(account string, err error) {
	// Cast err to awserr.Error to handle specific AWS errors
	aerr, ok := err.(awserr.Error)
	if ok && aerr.Code() == accessDeniedErrorCode {
		// The account does not have the role setup correctly
		log.Printf("The account '%s' denied access\n", account)
	} else if ok && aerr.Code() == unauthorizedErrorCode {
		log.Printf("Unauthorized to assume '%s'\n", account)
	} else if ok && aerr.Code() == notFoundErrorOcde {
		log.Printf("Resource was not found in account %s", account)
	} else if ok {
		// Some other AWS error occured
		log.Fatalf("Got AWS error in account %s: %s", account, aerr)
	} else {
		//Some other non-AWS error occured
		log.Fatalf("Got error in account %s: %s", account, err)
	}
}

func convertAWSTags(tags []*ec2.Tag) map[string]string {
	result := make(map[string]string)
	for _, tag := range tags {
		result[*tag.Key] = *tag.Value
	}
	return result
}

func convertAWSS3Tags(tags []*s3.Tag) map[string]string {
	result := make(map[string]string)
	for _, tag := range tags {
		result[*tag.Key] = *tag.Value
	}
	return result
}

func clientForAWSResource(res Resource) *ec2.EC2 {
	sess := session.Must(session.NewSession())
	creds := stscreds.NewCredentials(sess, fmt.Sprintf(assumeRoleARNTemplate, res.Owner()))
	return ec2.New(sess, &aws.Config{
		Credentials: creds,
		Region:      aws.String(res.Location()),
	})
}

func addAWSTag(r Resource, key, value string, overwrite bool) error {
	_, exist := r.Tags()[key]
	if exist && !overwrite {
		return fmt.Errorf("Key %s already exist on %s", key, r.ID())
	}
	client := clientForAWSResource(r)
	input := &ec2.CreateTagsInput{
		Resources: aws.StringSlice([]string{r.ID()}),
		Tags: []*ec2.Tag{&ec2.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		}},
	}
	_, err := client.CreateTags(input)
	return err
}

func removeAWSTag(r Resource, key string) error {
	val, exist := r.Tags()[key]
	if !exist {
		return nil
	}
	client := clientForAWSResource(r)
	input := &ec2.DeleteTagsInput{
		Resources: aws.StringSlice([]string{r.ID()}),
		Tags: []*ec2.Tag{&ec2.Tag{
			Key:   aws.String(key),
			Value: aws.String(val),
		}},
	}
	_, err := client.DeleteTags(input)
	return err
}
