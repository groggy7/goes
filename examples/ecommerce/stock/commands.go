package stock

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/modernice/goes/aggregate"
	"github.com/modernice/goes/command"
)

const (
	FillCommand    = "stock.fill"
	ReserveCommand = "stock.reserve"
	ReleaseCommand = "stock.release"
)

type FillPayload struct {
	Quantity int
}

type ReservePayload struct {
	OrderID  uuid.UUID
	Quantity int
}

type ReleasePayload struct {
	OrderID uuid.UUID
}

func Fill(productID uuid.UUID, quantity int) command.Command {
	return command.New(FillCommand, FillPayload{
		Quantity: quantity,
	}, command.Aggregate(AggregateName, productID))
}

func Reserve(productID, orderID uuid.UUID, quantity int) command.Command {
	return command.New(ReserveCommand, ReservePayload{
		OrderID:  orderID,
		Quantity: quantity,
	}, command.Aggregate(AggregateName, productID))
}

func Release(productID, orderID uuid.UUID) command.Command {
	return command.New(ReleaseCommand, ReleasePayload{
		OrderID: orderID,
	}, command.Aggregate(AggregateName, productID))
}

func RegisterCommands(r command.Registry) {
	r.Register(FillCommand, func() command.Payload { return FillPayload{} })
	r.Register(ReserveCommand, func() command.Payload { return ReservePayload{} })
	r.Register(ReleaseCommand, func() command.Payload { return ReleasePayload{} })
}

func HandleCommands(ctx context.Context, bus command.Bus, repo aggregate.Repository) (<-chan error, error) {
	parentCtx := ctx
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-parentCtx.Done()
		cancel()
	}()

	h := command.NewHandler(bus)

	if err := h.Handle(
		ctx, FillCommand,
		func(ctx command.Context) error {
			cmd := ctx.Command()
			load := cmd.Payload().(FillPayload)
			s := New(cmd.AggregateID())
			if err := repo.Fetch(ctx, s); err != nil {
				return fmt.Errorf("fetch Stock: %w", err)
			}
			if err := s.Fill(load.Quantity); err != nil {
				return fmt.Errorf("fill Stock: %w", err)
			}
			if err := repo.Save(ctx, s); err != nil {
				return fmt.Errorf("save Stock: %w", err)
			}
			return nil
		},
	); err != nil {
		cancel()
		return nil, fmt.Errorf("handle %q Commands: %w", FillCommand, err)
	}

	if err := h.Handle(
		ctx, ReserveCommand,
		func(ctx command.Context) error {
			cmd := ctx.Command()
			load := cmd.Payload().(ReservePayload)
			s := New(cmd.AggregateID())
			if err := repo.Fetch(ctx, s); err != nil {
				return fmt.Errorf("fetch Stock: %w", err)
			}
			if err := s.Reserve(load.Quantity, load.OrderID); err != nil {
				return fmt.Errorf("reserve Stock: %w", err)
			}
			if err := repo.Save(ctx, s); err != nil {
				return fmt.Errorf("save Stock: %w", err)
			}
			return nil
		},
	); err != nil {
		cancel()
		return nil, fmt.Errorf("handle %q Commands: %w", ReserveCommand, err)
	}

	if err := h.Handle(
		ctx, ReleaseCommand,
		func(ctx command.Context) error {
			cmd := ctx.Command()
			load := cmd.Payload().(ReleasePayload)
			s := New(cmd.AggregateID())
			if err := repo.Fetch(ctx, s); err != nil {
				return fmt.Errorf("fetch Stock: %w", err)
			}
			if err := s.Release(load.OrderID); err != nil {
				return err
			}
			if err := repo.Save(ctx, s); err != nil {
				return fmt.Errorf("save Stock: %w", err)
			}
			return nil
		},
	); err != nil {
		cancel()
		return nil, fmt.Errorf("handle %q Commands: %w", ReserveCommand, err)
	}

	return h.Errors(ctx), nil
}
