package searchsvc

import (
	"errors"
	"fmt"
	"io"
	"sync"

	objectV2 "github.com/nspcc-dev/neofs-api-go/v2/object"
	"github.com/nspcc-dev/neofs-api-go/v2/rpc"
	rpcclient "github.com/nspcc-dev/neofs-api-go/v2/rpc/client"
	"github.com/nspcc-dev/neofs-api-go/v2/session"
	"github.com/nspcc-dev/neofs-api-go/v2/signature"
	"github.com/nspcc-dev/neofs-node/pkg/core/client"
	"github.com/nspcc-dev/neofs-node/pkg/network"
	objectSvc "github.com/nspcc-dev/neofs-node/pkg/services/object"
	"github.com/nspcc-dev/neofs-node/pkg/services/object/internal"
	searchsvc "github.com/nspcc-dev/neofs-node/pkg/services/object/search"
	"github.com/nspcc-dev/neofs-node/pkg/services/object/util"
	cid "github.com/nspcc-dev/neofs-sdk-go/container/id"
	"github.com/nspcc-dev/neofs-sdk-go/object"
	oidSDK "github.com/nspcc-dev/neofs-sdk-go/object/id"
)

func (s *Service) toPrm(req *objectV2.SearchRequest, stream objectSvc.SearchStream) (*searchsvc.Prm, error) {
	meta := req.GetMetaHeader()

	commonPrm, err := util.CommonPrmFromV2(req)
	if err != nil {
		return nil, err
	}

	p := new(searchsvc.Prm)
	p.SetCommonParameters(commonPrm)

	p.SetWriter(&streamWriter{
		stream: stream,
	})

	if !commonPrm.LocalOnly() {
		var onceResign sync.Once

		key, err := s.keyStorage.GetKey(nil)
		if err != nil {
			return nil, err
		}

		p.SetRequestForwarder(groupAddressRequestForwarder(func(addr network.Address, c client.MultiAddressClient, pubkey []byte) ([]oidSDK.ID, error) {
			var err error

			// once compose and resign forwarding request
			onceResign.Do(func() {
				// compose meta header of the local server
				metaHdr := new(session.RequestMetaHeader)
				metaHdr.SetTTL(meta.GetTTL() - 1)
				// TODO: #1165 think how to set the other fields
				metaHdr.SetOrigin(meta)

				req.SetMetaHeader(metaHdr)

				err = signature.SignServiceMessage(key, req)
			})

			if err != nil {
				return nil, err
			}

			var searchStream *rpc.SearchResponseReader
			err = c.RawForAddress(addr, func(cli *rpcclient.Client) error {
				searchStream, err = rpc.SearchObjects(cli, req, rpcclient.WithContext(stream.Context()))
				return err
			})
			if err != nil {
				return nil, err
			}

			// code below is copy-pasted from c.SearchObjects implementation,
			// perhaps it is worth highlighting the utility function in neofs-api-go
			var (
				searchResult []oidSDK.ID
				resp         = new(objectV2.SearchResponse)
			)

			for {
				// receive message from server stream
				err := searchStream.Read(resp)
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}

					return nil, fmt.Errorf("reading the response failed: %w", err)
				}

				// verify response key
				if err = internal.VerifyResponseKeyV2(pubkey, resp); err != nil {
					return nil, err
				}

				// verify response structure
				if err := signature.VerifyServiceMessage(resp); err != nil {
					return nil, fmt.Errorf("could not verify %T: %w", resp, err)
				}

				chunk := resp.GetBody().GetIDList()
				for i := range chunk {
					searchResult = append(searchResult, *oidSDK.NewIDFromV2(&chunk[i]))
				}
			}

			return searchResult, nil
		}))
	}

	body := req.GetBody()
	p.WithContainerID(cid.NewFromV2(body.GetContainerID()))
	p.WithSearchFilters(object.NewSearchFiltersFromV2(body.GetFilters()))

	return p, nil
}

func groupAddressRequestForwarder(f func(network.Address, client.MultiAddressClient, []byte) ([]oidSDK.ID, error)) searchsvc.RequestForwarder {
	return func(info client.NodeInfo, c client.MultiAddressClient) ([]oidSDK.ID, error) {
		var (
			firstErr error
			res      []oidSDK.ID

			key = info.PublicKey()
		)

		info.AddressGroup().IterateAddresses(func(addr network.Address) (stop bool) {
			var err error

			defer func() {
				stop = err == nil

				if stop || firstErr == nil {
					firstErr = err
				}

				// would be nice to log otherwise
			}()

			res, err = f(addr, c, key)

			return
		})

		return res, firstErr
	}
}
