package uci

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"trollfish/stockfish"
)

const startPosFEN = "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"
const defaultThreads = 16
const threadsHashMultiplier = 512
const defaultMultiPV = 5
const agroMultiPV = 2

type UCI struct {
	name    string
	author  string
	options []Option

	fen string

	started int64

	moveListMtx       sync.Mutex
	moveListNodes     int
	moveList          []Info
	moveListPrinted   bool
	gameMoveCount     int
	gameActiveColor   string
	gameMultiPV       int
	gameMateIn        int
	gameEval          int
	gameEvalHumanized float64
	gameAgro          bool

	sf *stockfish.StockFish

	ctx    context.Context
	cancel context.CancelFunc

	mtxStdout sync.Mutex
	log       io.WriteCloser
}

type Info struct {
	Depth    int
	SelDepth int
	MultiPV  int
	Score    int
	Mate     int
	Nodes    int
	NPS      int
	HashFull int
	TBHits   int
	Time     int
	PV       string
}

func (m Info) String() string {
	var score string
	if m.Mate == 0 {
		score = fmt.Sprintf("cp %d", m.Score)
	} else {
		score = fmt.Sprintf("mate %d", m.Mate)
	}
	return fmt.Sprintf("depth %d seldepth %d multipv %d score %s nodes %d nps %d hashfull %d tbhits %d time %d pv %s",
		m.Depth, m.SelDepth, m.MultiPV, score, m.Nodes, m.NPS, m.HashFull, m.TBHits, m.Time, m.PV,
	)
}

func New(name, author string, options ...Option) *UCI {
	return &UCI{
		name:        name,
		author:      author,
		options:     options,
		gameMultiPV: defaultMultiPV,
	}
}

func (u *UCI) Start(ctx context.Context) (context.Context, context.CancelFunc) {
	if !atomic.CompareAndSwapInt64(&u.started, 0, 1) {
		return u.ctx, u.cancel
	}

	fp, err := os.OpenFile("trollfish.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}

	redirectStderr(fp)

	u.log = fp

	u.logInfo("=========================================")

	u.ctx, u.cancel = context.WithCancel(ctx)

	c := make(chan string, 512)

	go func() {
		defer close(c)
		r := bufio.NewScanner(os.Stdin)

		for r.Scan() {
			select {
			case c <- r.Text():
			case <-u.ctx.Done():
				_ = u.log.Close()
				return
			}
		}

		if err := r.Err(); err != nil {
			msg := fmt.Sprintf("info ERR: %v", err)
			u.WriteLine(msg)
		}
	}()

	// TODO: get path from config file
	sf, err := stockfish.Start(u.ctx, "/home/jud/projects/trollfish/stockfish/stockfish", u.logInfo)
	if err != nil {
		log.Fatal(err)
	}

	u.sf = sf

	go u.stockFishReadLoop()

	go func() {
		for line := range c {
			u.parseLine(line)
		}
	}()

	return u.ctx, u.cancel
}

func (u *UCI) logInfo(s string) {
	_, _ = u.log.Write([]byte(fmt.Sprintf("%s %s\n", ts(), s)))
}

func (u *UCI) stockFishReadLoop() {
	for line := range u.sf.Output {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, " ")
		if len(parts) == 0 {
			continue
		}

		cmd := parts[0]

		switch cmd {
		case "readyok":
			u.WriteLine("readyok")
		case "uciok":
			n := defaultThreads
			u.sf.Write(fmt.Sprintf("setoption name Threads value %d", n))
			u.sf.Write(fmt.Sprintf("setoption name Hash value %d", n*threadsHashMultiplier))
			u.sf.Write(fmt.Sprintf("setoption name MultiPV value %d", u.gameMultiPV))
			u.WriteLine("uciok")
		case "info":
			if parts[1] == "string" {
				// debug info, ignore
				break
			}

			var move Info
		infoLoop:
			for i := 1; i < len(parts); i += 2 {
				if i == len(parts)-1 {
					break
				}

				key := parts[i]

				var n int

				if key == "score" {
					if parts[i+1] == "cp" {
						key = "score.cp"
						n = atoi(parts[i+2])
					}
					if parts[i+1] == "mate" {
						key = "score.mate"
						n = atoi(parts[i+2])
					}
					i++
					if i+2 < len(parts) && (parts[i+2] == "lowerbound" || parts[i+2] == "upperbound") {
						// ignore
						i++
					}
				} else {
					n = atoi(parts[i+1])
				}

				switch key {
				case "score.cp":
					move.Score = n
				case "score.mate":
					move.Mate = n
				case "depth":
					move.Depth = n
				case "seldepth":
					move.SelDepth = n
				case "multipv":
					move.MultiPV = n
				case "nodes":
					move.Nodes = n
				case "nps":
					move.NPS = n
				case "hashfull":
					move.HashFull = n
				case "tbhits":
					move.TBHits = n
				case "time":
					move.Time = n
				case "currmove", "currmovenumber":
					// ignore
				case "pv":
					move.PV = strings.Join(parts[i+1:], " ")
					break infoLoop
				default:
					u.logInfo(fmt.Sprintf("unknown key '%s': %s", key, strings.Join(parts, " ")))
				}
			}

			if move.PV == "" {
				break
			}

			u.moveListMtx.Lock()
			if move.Nodes != u.moveListNodes {
				if len(u.moveList) > 0 {
					prevTime := u.moveList[0].Time
					timeDiff := move.Time - prevTime
					if timeDiff >= 10 {
						u.printMoveList(false)
					}
				}
				u.moveListNodes = move.Nodes
				u.moveList = nil
				u.moveListPrinted = false
			}
			u.moveList = append(u.moveList, move)
			u.moveListMtx.Unlock()

		case "bestmove":
			u.moveListMtx.Lock()

			minDist := 1_000_000

			var engineMove Info
			if len(u.moveList) > 0 {
				engineMove = u.moveList[0]
			} else {
				engineMove = Info{PV: strings.Join(parts[1:], " ")}
			}
			bestMove := engineMove

			engineMoveAbsEval := int(math.Abs(float64(engineMove.Score)))
			if engineMoveAbsEval > 2000 || engineMove.Mate > 0 || u.gameAgro {
				u.gameAgro = true
			} else {
				u.gameMateIn = 0

				for i := 0; i < len(u.moveList); i++ {
					move := u.moveList[i]
					if move.Mate < 0 {
						// don't get mated
						break
					}

					// avoid gross blunders
					if u.gameEval-move.Score > 250 {
						continue
					}

					// attempt to maintain equality until we hit agro
					dist := move.Score
					if dist < 0 {
						dist *= -1
					}
					if dist < minDist {
						bestMove = move
						minDist = dist
					}
				}
			}

			u.printMoveList(false)

			u.moveList = nil
			u.moveListPrinted = false
			u.moveListNodes = 0

			u.gameMateIn = bestMove.Mate
			u.gameEval = bestMove.Score

			u.moveListMtx.Unlock()

			uciMove := strings.Split(bestMove.PV, " ")[0]

			u.WriteLine(fmt.Sprintf("bestmove %s", uciMove))
			u.logInfo(fmt.Sprintf("agro: %v sf_move: %s sf_move_eval: %d played_move: %s eval: %d",
				u.gameAgro,
				strings.Split(engineMove.PV, " ")[0], engineMove.Score,
				uciMove, bestMove.Score,
			))

			evalHuman := float64(bestMove.Score) / 100
			if bestMove.Score != 0 && u.gameActiveColor == "b" {
				evalHuman *= -1
			}
			u.gameEvalHumanized = evalHuman

			u.WriteLine(fmt.Sprintf("info string agro %v eval %0.2f", u.gameAgro, u.gameEvalHumanized))

		default:
			u.logInfo(fmt.Sprintf("SF: <- %s", line))
			// TODO
		}
	}

	u.logInfo("stockfish read loop exited")
}

func (u *UCI) parseLine(line string) {
	u.logInfo(fmt.Sprintf("-> %s", line))

	parts := strings.Split(strings.TrimSpace(line), " ")
	if len(parts) == 0 {
		return
	}

	switch parts[0] {
	case "uci":
		u.SetUCI()
	case "quit":
		u.Quit()
	case "isready":
		u.sf.Write("isready")
	case "ucinewgame":
		u.sf.Write("ucinewgame")
		u.gameMoveCount = 0
		u.gameEval = 0
		u.gameMateIn = 0
		u.gameMultiPV = defaultMultiPV
		u.gameAgro = false
		u.gameActiveColor = "w"
		u.sf.Write(fmt.Sprintf("setoption name MultiPV value %d", u.gameMultiPV))
	case "setoption":
		if len(parts) > 4 {
			key := parts[2] // TODO: ignores that a key can be more than one word
			val := parts[4]
			u.SetOption(key, val)
		}
	case "position":
		u.SetPosition(parts[1:]...)
	case "stop":
		u.sf.Write(line)
	case "go":
		// TODO: handle 'infinite' and 'movetime <ms>'
		u.Go(parts[1:]...)
	case "":
	// no-op
	default:
		msg := fmt.Sprintf("info unknown command '%s'", parts[0])
		u.WriteLine(msg)
	}
}

func (u *UCI) Quit() {
	u.cancel()
	u.sf.Quit()
}

func (u *UCI) SetUCI() {
	var opts []string
	for _, o := range u.options {
		switch o.Type {
		case OptionTypeCheck:
		case OptionTypeSpin:
			opts = append(opts, fmt.Sprintf("option name %s type spin default %s min %d max %d", o.Name, o.DefaultValue(), o.Min, o.Max))
		case OptionTypeCombo:
		case OptionTypeButton:
		case OptionTypeString:
			opts = append(opts, fmt.Sprintf("option name %s type string default %s", o.Name, o.DefaultValue()))
		}
	}

	lines := make([]string, 0, len(opts)+3)

	lines = append(lines, fmt.Sprintf("id name %s", u.name))
	lines = append(lines, fmt.Sprintf("id author %s", u.author))
	lines = append(lines, "")
	lines = append(lines, opts...)

	u.WriteLines(lines...)

	u.sf.Write("uci")
}

func (u *UCI) SetOption(name, value string) {
	switch strings.ToLower(name) {
	case "threads":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			u.WriteLine(fmt.Sprintf("info option thread value %s invalid", value))
			return
		}

		u.sf.Write(fmt.Sprintf("setoption name Threads value %d", n))
		u.sf.Write(fmt.Sprintf("setoption name Hash value %d", n*threadsHashMultiplier))
		u.sf.Write(fmt.Sprintf("setoption name MultiPV value %d", u.gameMultiPV))
	case "multipv":
		// ignore
		//u.sf.Write(fmt.Sprintf("setoption name MultiPV value %s", value))
	default:
		u.WriteLine(fmt.Sprintf("info option '%s' not found", name))
	}
}

func (u *UCI) setOptionRaw(v ...string) {
	if len(v) == 0 {
		return
	}

	if v[0] != "name" {
		return
	}

	i := 1

	var name string
	for ; i < len(v); i++ {
		if v[i] == "value" {
			break
		}

		if name != "" {
			name += " "
		}
		name += v[i]
	}

	if i == len(v) || v[i] != "value" {
		// TODO: only valid for buttons
		return
	}

	var value string
	for ; i < len(v); i++ {
		if value != "" {
			value += " "
		}
		value += v[i]
	}
}

func (u *UCI) Go(v ...string) {
	// trollfish opening book
	if u.fen == startPosFEN {
		// 1. e4 (White, best (gambits) by test)
		u.WriteLine("bestmove e2e4")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w") {
		// 1. e4 e5 2. Qh5 (White, Wayward Queen)
		u.WriteLine("bestmove d1h5")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b") {
		// 1. e4 c5 (Black, Smith-Morra Gambit)
		u.WriteLine("bestmove c7c5")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pp1ppppp/8/2p5/4P3/8/PPPP1PPP/RNBQKBNR w") {
		// 1. e4 c5 2. d4 (White, Smith-Morra Gambit)
		u.WriteLine("bestmove d2d4")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pp1ppppp/8/2p5/3PP3/8/PPP2PPP/RNBQKBNR b") {
		// 1. e4 c5 2. d4 cxd4 (Black, Smith-Morra Gambit)
		u.WriteLine("bestmove c5d4")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pp1ppppp/8/8/3pP3/8/PPP2PPP/RNBQKBNR w") {
		// 1. e4 c5 2. d4 cxd4 3. c3 (White, Smith-Morra Gambit)
		u.WriteLine("bestmove c2c3")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pp1ppppp/8/8/3pP3/2P5/PP3PPP/RNBQKBNR b") {
		// 1. e4 c5 2. d4 cxd4 3. c3 dxc3 (Black, Smith-Morra Gambit)
		u.WriteLine("bestmove d4c3")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pp1ppppp/8/8/4P3/2p5/PP3PPP/RNBQKBNR w") {
		// 1. e4 c5 2. d4 cxd4 3. c3 dxc3 4. Nxc3 (White, Smith-Morra Gambit)
		u.WriteLine("bestmove b1c3")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pppppppp/8/8/3P4/8/PPP1PPPP/RNBQKBNR b") {
		// 1. d4 e5 (Black, Englund Gambit)
		u.WriteLine("bestmove e7e5")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pppp1ppp/8/4p3/3P4/8/PPP1PPPP/RNBQKBNR w") {
		// 1. d4 e5 2. dxe5 (White, Englund Gambit)
		u.WriteLine("bestmove d4e5")
		return
	}

	if strings.HasPrefix(u.fen, "rnbqkbnr/pppp1ppp/8/4P3/8/8/PPP1PPPP/RNBQKBNR b") {
		// 1. d4 e5 2. dxe5 Nc6 (Black, Englund Gambit)
		u.WriteLine("bestmove b8c6")
		return
	}

	if strings.HasPrefix(u.fen, "r1bqkbnr/pppp1ppp/2n5/4P3/8/8/PPP1PPPP/RNBQKBNR w") {
		// 1. d4 e5 2. dxe5 Nc6 3. Nf3 (White, Englund Gambit)
		u.WriteLine("bestmove g1f3")
		return
	}

	if strings.HasPrefix(u.fen, "r1bqkbnr/pppp1ppp/2n5/4P3/8/5N2/PPP1PPPP/RNBQKB1R b") { // 3. Nf3
		// 1. d4 e5 2. dxe5 Nc6 3. Nf3 Qe7 (Black, Englund Gambit)
		u.WriteLine("bestmove d8e7")
		return
	}

	if strings.HasPrefix(u.fen, "r1bqkbnr/pppp1ppp/2n5/4P3/5B2/8/PPP1PPPP/RN1QKBNR b") { // 3. Bf4
		// 1. d4 e5 2. dxe5 Nc6 3. Bf4 Qe7 (Black, Englund Gambit)
		u.WriteLine("bestmove d8e7")
		return
	}

	if strings.HasPrefix(u.fen, "r1b1kbnr/ppppqppp/2n5/4P3/8/5N2/PPP1PPPP/RNBQKB1R w") { // 4. Bg5
		// 1. d4 e5 2. dxe5 Nc6 3. Nf3 Qe7 4. Bg5 (White, Englund Gambit)
		u.WriteLine("bestmove c1g5")
		return
	}

	if strings.HasPrefix(u.fen, "r1b1kbnr/ppppqppp/2n5/4P1B1/8/5N2/PPP1PPPP/RN1QKB1R b") { // 4. Bg5 Qb4+
		// 1. d4 e5 2. dxe5 Nc6 3. Nf3 Qe7 4. Bg5 Qb4+ (Black, Englund Gambit)
		u.WriteLine("bestmove e7b4")
		return
	}

	if strings.HasPrefix(u.fen, "r1b1kbnr/ppppqppp/2n5/4P3/5B2/5N2/PPP1PPPP/RN1QKB1R b") { // (Nf3, Bf4) ... Qb4+
		// 1. d4 e5 2. dxe5 Nc6 3. Nf3 Qe7 4. Bg4 Qb4+ (Black, Englund Gambit)
		u.WriteLine("bestmove e7b4")
		return
	}

	if strings.HasPrefix(u.fen, "r1b1kbnr/pppp1ppp/2n5/4P1B1/1q6/2N2N2/PPP1PPPP/R2QKB1R b") { // Bg5 Nc3
		// 1. d4 e5 2. dxe5 Nc6 3. Nf3 Qe7 4. Bg4 Qb4+ 5. Nc3 Qxc2 (Black, Englund Gambit)
		u.WriteLine("bestmove b4b2")
		return
	}

	if strings.HasPrefix(u.fen, "r1b1kbnr/pppp1ppp/2n5/4P3/8/2N2N2/PqPBPPPP/R2QKB1R b") { // Bc2 Bb4
		u.WriteLine("bestmove f8b4")
		return
	}

	// TODO: play against humans
	/*if strings.HasPrefix(u.fen, "r1b1k1nr/pppp1ppp/2n5/4P3/1b6/2N2N2/PqPBPPPP/1R1QKB1R b") { // Bc2 Bb4 Rb1 ... sac!
		u.WriteLine("bestmove b2c3")
		return
	}*/

	if strings.HasPrefix(u.fen, "r1b1kbnr/pppp1ppp/2n5/4P3/1q6/5N2/PPPBPPPP/RN1QKB1R b") {
		// 1. d4 e5 2. dxe5 Nc6 3. Nf3 Qe7 4. (Bg4, Bg5) Qb4+ 5. Bd2 Qxc2 (Black, Englund Gambit)
		u.WriteLine("bestmove b4b2")
		return
	}

	// passthroughs
	if len(v) <= 1 {
		u.sf.Write(fmt.Sprintf("go %s", strings.Join(v, " ")))
		return
	}

	if v[0] != "wtime" {
		u.sf.Write(fmt.Sprintf("go %s", strings.Join(v, " ")))
		return
	}

	var wtime, btime, winc, binc int
	for i := 0; i < len(v); i += 2 {
		switch v[i] {
		case "wtime":
			wtime = atoi(v[i+1])
		case "winc":
			binc = atoi(v[i+1])
		case "btime":
			btime = atoi(v[i+1])
		case "binc":
			binc = atoi(v[i+1])
		default:

		}
	}

	var ourTime, oppTime, ourInc, oppInc int
	if u.gameActiveColor == "w" {
		ourTime, ourInc = wtime, winc
		oppTime, oppInc = btime, binc
	} else {
		oppTime, oppInc = wtime, winc
		ourTime, ourInc = btime, binc
	}

	lowTime := ourTime < 15_000
	veryLowTime := lowTime && (ourTime < 5_000 || ourTime < oppTime)

	u.sf.Write(fmt.Sprintf("info string our_time: %d+%d opp_time: %d+%d active_color: %s %v low_time: %v very_low_time: %v",
		ourTime, ourInc, oppTime, oppInc, u.gameActiveColor, v, lowTime, veryLowTime))

	// don't tell SF we're in a time control
	// TODO: improve time management
	agro := false

	moveTime := 500 + rand.Intn(1000)

	if u.gameMoveCount < 5 {
		moveTime = 100 + rand.Intn(500)
	} else if u.gameMateIn > 0 {
		agro = true
		moveTime = max(250, 100*u.gameMateIn)
	} else if u.gameEval > 800 {
		agro = true
	} else if u.gameMoveCount >= 30 && u.gameMoveCount < 40 {
		if u.gameEval < 150 {
			agro = true
			moveTime = 2000 + rand.Intn(1000)
		}
	} else if u.gameMoveCount >= 40 {
		agro = true
		if u.gameEval == 0 {
			moveTime = 250 // flag 'em
		} else if u.gameEval < 350 {
			moveTime = 3500 + rand.Intn(1000)
		}
	}

	// we're losing, stop to think
	if u.gameEval < -300 && ourTime > (oppTime/2) {
		moveTime = 3500 + rand.Intn(1000)
	}

	if veryLowTime {
		moveTime = min(moveTime, 50)
	} else if lowTime {
		moveTime = min(moveTime, 250)
	}

	if agro {
		u.gameAgro = true
		if u.gameMultiPV != agroMultiPV {
			u.gameMultiPV = agroMultiPV
			u.sf.Write(fmt.Sprintf("setoption name MultiPV value %d", u.gameMultiPV))
		}
	}

	u.sf.Write(fmt.Sprintf("go movetime %d", moveTime))
}

func (u *UCI) SetPosition(v ...string) {
	if len(v) == 0 {
		return
	}

	cmd := v[0]

	u.sf.Write(fmt.Sprintf("position %s", strings.Join(v, " ")))

	if cmd == "fen" {
		var fenEnd int
		for fenEnd = 1; fenEnd < len(v); fenEnd++ {
			if v[fenEnd] == "moves" {
				break
			}
		}
		u.fen = strings.Join(v[1:fenEnd], " ")
		b := FENtoBoard(u.fen)
		if len(v) != fenEnd && v[fenEnd] == "moves" {
			moves := v[fenEnd+1:]
			b.Moves(moves...)
		}
		u.fen = b.FEN()
		u.gameMoveCount = atoi(b.FullMove)
		u.gameActiveColor = b.ActiveColor

		u.WriteLine(fmt.Sprintf("info fen set to '%s' move %d, %s to play", u.fen, u.gameMoveCount, u.gameActiveColor))
		return
	}

	if cmd != "startpos" {
		// unknown
		u.WriteLine(fmt.Sprintf("info ERR: position '%s' command unknown", cmd))
		return
	}

	u.fen = startPosFEN
	u.WriteLine(fmt.Sprintf("info fen set to '%s'", u.fen))

	if len(v) == 1 {
		return
	}

	cmd = v[1]

	if cmd != "moves" {
		u.WriteLine(fmt.Sprintf("info ERR: position startpos '%s' command unknown", cmd))
		return
	}

	moves := v[2:]

	b := FENtoBoard(u.fen)
	b.Moves(moves...)
	u.fen = b.FEN()
	u.gameActiveColor = b.ActiveColor

	u.gameMoveCount = atoi(b.FullMove)
}

func (u *UCI) printMoveList(lock bool) {
	if lock {
		u.moveListMtx.Lock()
		defer u.moveListMtx.Unlock()
	}

	if u.moveListPrinted {
		return
	}

	pvs := make([]string, 0, len(u.moveList))
	for _, move := range u.moveList {
		pvs = append(pvs, fmt.Sprintf("info %s", move.String()))
	}
	u.WriteLines(pvs...)

	u.moveListPrinted = true
}

func (u *UCI) WriteLine(s string) {
	u.mtxStdout.Lock()
	defer u.mtxStdout.Unlock()
	u.logInfo(fmt.Sprintf("<- %s", s))
	_, _ = fmt.Fprintln(os.Stdout, s)
}

func (u *UCI) WriteLines(v ...string) {
	var w strings.Builder
	for _, s := range v {
		w.WriteString(s)
		w.WriteRune('\n')

		u.logInfo("<- " + s)
	}
	s := w.String()

	u.mtxStdout.Lock()
	defer u.mtxStdout.Unlock()
	_, _ = fmt.Fprint(os.Stdout, s)
}

func ts() string {
	return fmt.Sprintf("[%s]", time.Now().Format("2006-01-02 15:04:05"))
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// redirectStderr to the file passed in
func redirectStderr(f *os.File) {
	err := syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd()))
	if err != nil {
		log.Fatalf("Failed to redirect stderr to file: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
