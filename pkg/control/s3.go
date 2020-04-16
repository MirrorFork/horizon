package control

import (
	bytes "bytes"
	context "context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	fmt "fmt"
	"time"

	"cirello.io/dynamolock"
	"github.com/DataDog/zstd"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/hashicorp/horizon/pkg/dbx"
	"github.com/hashicorp/horizon/pkg/pb"
	"github.com/jinzhu/gorm"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
)

func (s *Server) calculateAccountRouting(ctx context.Context, account []byte) ([]byte, error) {
	var u ulid.ULID
	copy(u[:], account)

	var lastId int64

	services := make([]*Service, 0, 100)

	var accountServices pb.AccountServices

	// See https://www.citusdata.com/blog/2016/03/30/five-ways-to-paginate/ for decisions
	// on why we use this particular method to get all the records for an account. It's
	// important to note that there is an index on (account,id) on the services table
	// that allows this query to scan the index it's natural order.
	for {
		// Gotta poll the context since database/sql and gorm don't expose a context
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		err := dbx.Check(s.db.Where("account_id = ?", account).Where("id > ?", lastId).Limit(100).Find(&services))
		if err != nil {
			if err == gorm.ErrRecordNotFound {
				break
			}
		}

		if len(services) == 0 {
			break
		}

		for _, serv := range services {
			accountServices.Services = append(accountServices.Services, &pb.ServiceRoute{
				Hub:       pb.ULIDFromBytes(serv.HubId),
				Id:        pb.ULIDFromBytes(serv.ServiceId),
				Type:      serv.Type,
				LabelSets: ExplodeLabelSetss(serv.Labels),
			})
		}

		lastId = services[len(services)-1].ID

		services = services[:0]
	}

	data, err := accountServices.Marshal()
	if err != nil {
		return nil, err
	}

	return zstd.Compress(nil, data)
}

func (s *Server) updateAccountRouting(ctx context.Context, account []byte) error {
	var u ulid.ULID
	copy(u[:], account)

	outData, err := s.calculateAccountRouting(ctx, account)
	if err != nil {
		return err
	}

	h := md5.New()
	h.Write(outData)
	sum := h.Sum(nil)

	key := fmt.Sprintf("account_services/%s", u.String())

	lockKey := "account-" + u.String()

	strMD5 := base64.StdEncoding.EncodeToString(sum)

	for {
		lock, err := s.lockMgr.AcquireLock(lockKey,
			dynamolock.WithAdditionalAttributes(
				map[string]*dynamodb.AttributeValue{
					"md5": {S: &strMD5},
				}),
			dynamolock.FailIfLocked(),
		)

		if err == nil {
			defer lock.Close()
			break
		}

		info, err := s.lockMgr.Get(lockKey)
		if err != nil {
			return err
		}

		attrs := info.AdditionalAttributes()
		if val, ok := attrs["md5"]; ok {
			if val.S != nil && *val.S == strMD5 {
				// Ok, someone else got all the records, PEACE OUT.
				return nil
			}
		}

		time.Sleep(5 * time.Second)

		outData, err := s.calculateAccountRouting(ctx, account)
		if err != nil {
			return err
		}

		h := md5.New()
		h.Write(outData)
		sum := h.Sum(nil)

		strMD5 = base64.StdEncoding.EncodeToString(sum)
		continue
	}

	s3obj := s3.New(s.awsSess)

	inputEtag := base64.StdEncoding.EncodeToString(sum)

	putIn := &s3.PutObjectInput{
		ACL:         aws.String("private"),
		Body:        bytes.NewReader(outData),
		ContentMD5:  aws.String(inputEtag),
		ContentType: aws.String("application/horizon"),
		Bucket:      &s.bucket,
		Key:         &key,
		Tagging:     aws.String("usage=horizon"),
	}

	if s.kmsKeyId != "" {
		putIn.SSEKMSKeyId = aws.String(s.kmsKeyId)
		putIn.ServerSideEncryption = aws.String("aws:kms")
	}

	putOut, err := s3obj.PutObject(putIn)
	if err != nil {
		return errors.Wrapf(err, "unable to upload object")
	}

	outet := *putOut.ETag

	outSum, err := hex.DecodeString(outet[1 : len(outet)-1])
	if err != nil {
		return err
	}

	if !bytes.Equal(sum, outSum) {
		return fmt.Errorf("corruption detected, wrong etag: %s / %s", hex.EncodeToString(sum), outet)
	}

	return s.s.UserEvent("account-updated", account, false)
}

func (s *Server) updateLabelLinks(ctx context.Context) error {
	lastId := 0

	lls := make([]*LabelLink, 0, 100)

	var out pb.LabelLinks

	for {
		// Gotta poll the context since database/sql and gorm don't expose a context
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := dbx.Check(s.db.Where("id > ?", lastId).Limit(100).Find(&lls))
		if err != nil {
			if err == gorm.ErrRecordNotFound {
				break
			}
		}

		if len(lls) == 0 {
			break
		}

		for _, ll := range lls {
			out.LabelLinks = append(out.LabelLinks, &pb.LabelLink{
				Account: &pb.Account{
					AccountId: pb.ULIDFromBytes(ll.AccountID),
				},
				Labels: ExplodeLabels(ll.Labels),
				Target: ExplodeLabels(ll.Target),
			})
		}

		lastId = lls[len(lls)-1].ID

		lls = lls[:0]
	}

	data, err := out.Marshal()
	if err != nil {
		return err
	}

	outData, err := zstd.Compress(nil, data)
	if err != nil {
		return err
	}

	h := md5.New()
	h.Write(outData)
	sum := h.Sum(nil)

	s3obj := s3.New(s.awsSess)

	inputEtag := base64.StdEncoding.EncodeToString(sum)

	putIn := &s3.PutObjectInput{
		ACL:         aws.String("private"),
		Body:        bytes.NewReader(outData),
		ContentMD5:  aws.String(inputEtag),
		ContentType: aws.String("application/horizon"),
		Bucket:      &s.bucket,
		Key:         aws.String("label_links"),
		Tagging:     aws.String("usage=horizon"),
	}

	if s.kmsKeyId != "" {
		putIn.SSEKMSKeyId = aws.String(s.kmsKeyId)
		putIn.ServerSideEncryption = aws.String("aws:kms")
	}

	putOut, err := s3obj.PutObject(putIn)
	if err != nil {
		return errors.Wrapf(err, "unable to upload object")
	}

	outet := *putOut.ETag

	outSum, err := hex.DecodeString(outet[1 : len(outet)-1])
	if err != nil {
		return err
	}

	if !bytes.Equal(sum, outSum) {
		return fmt.Errorf("corruption detected, wrong etag: %s / %s", hex.EncodeToString(sum), outet)
	}

	/*
		uploader := s3manager.NewUploader(s.awsSess)
		_, err = uploader.UploadWithContext(ctx, &s3manager.UploadInput{
			ACL:                  aws.String("private"),
			Body:                 bytes.NewReader(outData),
			ContentMD5:           aws.String(base64.StdEncoding.EncodeToString(sum)),
			ContentType:          aws.String("application/horizon"),
			Bucket:               &s.bucket,
			Key:                  aws.String("label-links"),
			SSEKMSKeyId:          aws.String(s.kmsKeyId),
			ServerSideEncryption: aws.String("aws:kms"),
			Tagging:              aws.String("usage=horizon"),
		})
	*/

	return s.s.UserEvent("label-link-updated", nil, true)
}
