"use client";

import React, { useState, useEffect, useRef } from "react";
import {
  Play,
  Pause,
  Sliders,
  Activity,
  History,
  Terminal,
  Cpu,
  TrendingUp,
  TrendingDown,
  DollarSign,
  AlertTriangle,
  RefreshCw,
  Layers,
  ArrowUpRight,
  ArrowDownRight,
  ShieldCheck,
  Settings,
  HelpCircle,
  X
} from "lucide-react";

// Types matching backend models
interface RunnerConfig {
  instrument: string;
  allora_topic_id: number;
  granularity: string;
  risk_percent: number;
  atr_multiplier: number;
  tp_multiplier: number;
  ema_fast_period: number;
  ema_slow_period: number;
  rsi_period: number;
  min_rsi_filter: number;
  max_rsi_filter: number;
  trading_enabled: boolean;
  use_allora: boolean;
  default_pip_value: number;
}

interface StatusData {
  runner_config: RunnerConfig;
  environment: string;
  balance: number;
  equity: number;
  timestamp: string;
}

interface Position {
  id: string;
  instrument: string;
  units: number;
  open_price: number;
  open_time: string;
  stop_loss: number;
  take_profit: number;
  status: string;
  close_price?: number;
  close_time?: string;
  realized_pnl?: number;
}

interface Transaction {
  id: string;
  type: string; // BUY, SELL, CLOSE
  instrument: string;
  price: number;
  units: number;
  realized_pnl: number;
  timestamp: string;
}

interface SystemLog {
  id: number;
  level: string; // INFO, WARN, ERROR
  message: string;
  timestamp: string;
}

interface AlloraInference {
  topic_id: number;
  block_height: number;
  combined_value: string;
  parsed_value: number;
  timestamp: string;
}

interface Candle {
  time: string;
  volume: number;
  open: number;
  high: number;
  low: number;
  close: number;
}

export default function Home() {
  // Connection state
  const [backendURL, setBackendURL] = useState("http://localhost:8080");
  const [isConnected, setIsConnected] = useState(false);
  const [isConnecting, setIsConnecting] = useState(true);

  // Bot states
  const [status, setStatus] = useState<StatusData | null>(null);
  const [positions, setPositions] = useState<Position[]>([]);
  const [trades, setTrades] = useState<Transaction[]>([]);
  const [logs, setLogs] = useState<SystemLog[]>([]);
  const [inferences, setInferences] = useState<AlloraInference[]>([]);
  const [candles, setCandles] = useState<Candle[]>([]);

  // Local Form state
  const [instrument, setInstrument] = useState("EUR_USD");
  const [alloraTopicID, setAlloraTopicID] = useState(1);
  const [riskPercent, setRiskPercent] = useState(1.0);
  const [atrMultiplier, setAtrMultiplier] = useState(2.0);
  const [tpMultiplier, setTpMultiplier] = useState(3.0);
  const [emaFast, setEmaFast] = useState(10);
  const [emaSlow, setEmaSlow] = useState(25);
  const [tradingEnabled, setTradingEnabled] = useState(true);
  const [useAllora, setUseAllora] = useState(true);

  // Manual trade input state
  const [manualUnits, setManualUnits] = useState(10000);
  const [manualStopLoss, setManualStopLoss] = useState(0);
  const [manualTakeProfit, setManualTakeProfit] = useState(0);

  // User Guide Modal state
  const [showManual, setShowManual] = useState(false);

  // Log terminal container ref
  const terminalContainerRef = useRef<HTMLDivElement>(null);

  // Fetch initial & interval data
  const fetchData = async () => {
    try {
      // Test status
      const statusRes = await fetch(`${backendURL}/api/status`);
      if (!statusRes.ok) throw new Error("Backend unreachable");
      const statusData: StatusData = await statusRes.json();
      setStatus(statusData);
      setIsConnected(true);

      // Populate config form inputs once on first load
      if (!status) {
        setInstrument(statusData.runner_config.instrument);
        setAlloraTopicID(statusData.runner_config.allora_topic_id);
        setRiskPercent(statusData.runner_config.risk_percent);
        setAtrMultiplier(statusData.runner_config.atr_multiplier);
        setTpMultiplier(statusData.runner_config.tp_multiplier);
        setEmaFast(statusData.runner_config.ema_fast_period);
        setEmaSlow(statusData.runner_config.ema_slow_period);
        setTradingEnabled(statusData.runner_config.trading_enabled);
        setUseAllora(statusData.runner_config.use_allora);
      }

      // Fetch positions
      const posRes = await fetch(`${backendURL}/api/positions`);
      const posData = await posRes.json();
      setPositions(Array.isArray(posData) ? posData : []);

      // Fetch trades
      const tradesRes = await fetch(`${backendURL}/api/trades`);
      const tradesData = await tradesRes.json();
      setTrades(Array.isArray(tradesData) ? tradesData : []);

      // Fetch logs
      const logsRes = await fetch(`${backendURL}/api/logs?limit=40`);
      const logsData = await logsRes.json();
      setLogs(Array.isArray(logsData) ? logsData : []);

      // Fetch inferences
      const infRes = await fetch(`${backendURL}/api/inferences?limit=20`);
      const infData = await infRes.json();
      setInferences(Array.isArray(infData) ? infData : []);

      // Fetch candles
      const candlesRes = await fetch(`${backendURL}/api/candles?count=50`);
      const candlesData = await candlesRes.json();
      setCandles(Array.isArray(candlesData) ? candlesData : []);

    } catch (err) {
      setIsConnected(false);
    } finally {
      setIsConnecting(false);
    }
  };

  useEffect(() => {
    fetchData();
    const interval = setInterval(fetchData, 1500); // 1.5s real-time fast-polling
    return () => clearInterval(interval);
  }, [backendURL, status === null]);

  useEffect(() => {
    if (terminalContainerRef.current) {
      const container = terminalContainerRef.current;
      container.scrollTop = container.scrollHeight;
    }
  }, [logs]);

  // Handle bot config update
  const handleUpdateConfig = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!status) return;

    const payload: RunnerConfig = {
      ...status.runner_config,
      instrument,
      allora_topic_id: Number(alloraTopicID),
      risk_percent: Number(riskPercent),
      atr_multiplier: Number(atrMultiplier),
      tp_multiplier: Number(tpMultiplier),
      ema_fast_period: Number(emaFast),
      ema_slow_period: Number(emaSlow),
      trading_enabled: tradingEnabled,
      use_allora: useAllora,
    };

    try {
      const res = await fetch(`${backendURL}/api/config`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });

      if (res.ok) {
        alert("Configuration updated successfully!");
        fetchData();
      } else {
        alert("Failed to update config.");
      }
    } catch (err) {
      alert("Error sending update request.");
    }
  };

  // Toggle Bot Status
  const handleToggleTrading = async (enabled: boolean) => {
    if (!status) return;
    const payload = {
      ...status.runner_config,
      trading_enabled: enabled,
    };
    try {
      await fetch(`${backendURL}/api/config`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });
      setTradingEnabled(enabled);
      fetchData();
    } catch (err) {
      console.error(err);
    }
  };

  // Close Position
  const handleClosePosition = async (posID: string, currentPrice: number) => {
    if (!confirm("Are you sure you want to close this position?")) return;
    try {
      const res = await fetch(`${backendURL}/api/trade/close`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ position_id: posID, current_price: currentPrice }),
      });
      if (res.ok) {
        alert("Position closed!");
        fetchData();
      } else {
        alert("Failed to close position.");
      }
    } catch (err) {
      console.error(err);
    }
  };

  // Submit Manual Trade
  const handleManualTrade = async (type: "BUY" | "SELL") => {
    if (candles.length === 0) return;
    const currentPrice = candles[candles.length - 1].close;

    const units = type === "BUY" ? manualUnits : -manualUnits;

    try {
      const res = await fetch(`${backendURL}/api/trade/manual`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          instrument: status?.runner_config.instrument || "EUR_USD",
          units,
          price: currentPrice,
          stop_loss: Number(manualStopLoss),
          take_profit: Number(manualTakeProfit),
        }),
      });

      if (res.ok) {
        alert(`Manual ${type} order submitted successfully!`);
        setManualStopLoss(0);
        setManualTakeProfit(0);
        fetchData();
      } else {
        const errJson = await res.json();
        alert(`Order failed: ${errJson.error}`);
      }
    } catch (err) {
      console.error(err);
      alert("Error sending order request.");
    }
  };

  // SVG Chart Calculations
  const renderChart = () => {
    if (candles.length === 0) {
      return (
        <div className="h-64 flex items-center justify-center text-slate-500 bg-slate-950/40 rounded-xl border border-slate-900">
          No candle data available. Ensure Oanda key is valid or backend is running.
        </div>
      );
    }

    // Chart Dimensions
    const width = 800;
    const height = 300;
    const paddingLeft = 10;
    const paddingRight = 60;
    const paddingTop = 20;
    const paddingBottom = 30;

    const chartW = width - paddingLeft - paddingRight;
    const chartH = height - paddingTop - paddingBottom;

    // Find min and max prices
    let minPrice = Math.min(...candles.map(c => c.low));
    let maxPrice = Math.max(...candles.map(c => c.high));

    // Pad prices a bit
    const spread = maxPrice - minPrice;
    minPrice -= spread * 0.05;
    maxPrice += spread * 0.05;

    // Mapping functions
    const xMap = (index: number) => paddingLeft + (index / (candles.length - 1)) * chartW;
    const yMap = (price: number) => height - paddingBottom - ((price - minPrice) / (maxPrice - minPrice)) * chartH;

    // Calculate indicator EMAs locally for drawing overlay
    const fastPeriod = status?.runner_config.ema_fast_period || 10;
    const slowPeriod = status?.runner_config.ema_slow_period || 25;

    const fastEmaVals = calculateEmaArray(candles.map(c => c.close), fastPeriod);
    const slowEmaVals = calculateEmaArray(candles.map(c => c.close), slowPeriod);

    return (
      <div className="relative bg-slate-950/80 rounded-xl p-4 border border-slate-800/60 shadow-inner">
        <div className="absolute top-2 left-4 text-xs font-semibold text-slate-400 bg-slate-900/80 py-1 px-2.5 rounded-full border border-slate-800">
          {status?.runner_config.instrument} • {status?.runner_config.granularity} Candles
        </div>

        <svg width="100%" height={height} viewBox={`0 0 ${width} ${height}`} className="overflow-visible">
          {/* Grid lines */}
          {[0, 0.25, 0.5, 0.75, 1].map((ratio, i) => {
            const price = maxPrice - ratio * (maxPrice - minPrice);
            const y = yMap(price);
            return (
              <g key={i}>
                <line x1={paddingLeft} y1={y} x2={width - paddingRight} y2={y} stroke="#1e293b" strokeDasharray="3,3" />
                <text x={width - paddingRight + 5} y={y + 4} fill="#64748b" className="text-[10px] font-mono">
                  {price.toFixed(5)}
                </text>
              </g>
            );
          })}

          {/* Candlesticks */}
          {candles.map((candle, idx) => {
            const x = xMap(idx);
            const yOpen = yMap(candle.open);
            const yClose = yMap(candle.close);
            const yHigh = yMap(candle.high);
            const yLow = yMap(candle.low);

            const isGreen = candle.close >= candle.open;
            const strokeColor = isGreen ? "#10b981" : "#f43f5e";
            const fillColor = isGreen ? "rgba(16,185,129,0.3)" : "rgba(244,63,94,0.3)";

            const candleW = Math.max(2, chartW / candles.length - 4);

            return (
              <g key={idx}>
                {/* Wick */}
                <line x1={x} y1={yHigh} x2={x} y2={yLow} stroke={strokeColor} strokeWidth="1.5" />
                {/* Body */}
                <rect
                  x={x - candleW / 2}
                  y={Math.min(yOpen, yClose)}
                  width={candleW}
                  height={Math.max(1, Math.abs(yOpen - yClose))}
                  fill={fillColor}
                  stroke={strokeColor}
                  strokeWidth="1.5"
                  rx="1"
                />
              </g>
            );
          })}

          {/* Fast EMA Line */}
          {fastEmaVals.length > 0 && (
            <path
              d={fastEmaVals
                .map((val, idx) => (val > 0 ? `${idx === fastPeriod - 1 ? "M" : "L"} ${xMap(idx)} ${yMap(val)}` : ""))
                .join(" ")}
              fill="none"
              stroke="#6366f1"
              strokeWidth="1.5"
              strokeDasharray="2,2"
            />
          )}

          {/* Slow EMA Line */}
          {slowEmaVals.length > 0 && (
            <path
              d={slowEmaVals
                .map((val, idx) => (val > 0 ? `${idx === slowPeriod - 1 ? "M" : "L"} ${xMap(idx)} ${yMap(val)}` : ""))
                .join(" ")}
              fill="none"
              stroke="#f59e0b"
              strokeWidth="1.5"
            />
          )}
        </svg>

        <div className="flex items-center gap-4 mt-2 justify-end text-[10px] text-slate-400 font-mono">
          <div className="flex items-center gap-1.5">
            <span className="w-3 h-0.5 bg-[#6366f1] inline-block dashed"></span> Fast EMA ({fastPeriod})
          </div>
          <div className="flex items-center gap-1.5">
            <span className="w-3 h-0.5 bg-[#f59e0b] inline-block"></span> Slow EMA ({slowPeriod})
          </div>
          <div className="flex items-center gap-1.5">
            <span className="w-2.5 h-2.5 bg-emerald-500/30 border border-emerald-500 rounded-sm inline-block"></span> Bullish Candle
          </div>
          <div className="flex items-center gap-1.5">
            <span className="w-2.5 h-2.5 bg-rose-500/30 border border-rose-500 rounded-sm inline-block"></span> Bearish Candle
          </div>
        </div>
      </div>
    );
  };

  return (
    <div className="flex-1 bg-slate-950 text-slate-100 flex flex-col font-sans selection:bg-emerald-500 selection:text-slate-950">
      {/* Glow effect backgrounds */}
      <div className="absolute top-0 left-1/4 w-[500px] h-[500px] bg-emerald-500/5 rounded-full blur-[120px] pointer-events-none" />
      <div className="absolute top-1/3 right-10 w-[400px] h-[400px] bg-indigo-500/5 rounded-full blur-[100px] pointer-events-none" />

      {/* Top Navbar */}
      <header className="border-b border-slate-900 bg-slate-950/80 backdrop-blur-md sticky top-0 z-50 px-6 py-4 flex items-center justify-between">
        <div className="flex items-center gap-3">
          <div className="bg-gradient-to-tr from-emerald-500 to-teal-400 p-2 rounded-xl text-slate-950 shadow-lg shadow-emerald-500/20">
            <Activity className="w-6 h-6 animate-pulse" />
          </div>
          <div>
            <h1 className="text-lg font-black tracking-wider text-slate-100 flex items-center gap-2">
              TIQ <span className="text-[10px] bg-slate-900 border border-slate-800 text-slate-400 py-0.5 px-2 rounded-md font-mono uppercase tracking-normal">TIQ AI</span>
            </h1>
            <p className="text-[10px] text-slate-500">Autonomous Oanda Forex Agent</p>
          </div>
        </div>

        {/* Backend Connect status */}
        <div className="flex items-center gap-4">
          <button
            onClick={() => setShowManual(true)}
            className="flex items-center gap-1.5 bg-slate-900/60 hover:bg-slate-800 border border-slate-800 hover:border-slate-700 px-3.5 py-1.5 rounded-lg text-xs font-semibold text-slate-300 transition active:scale-95"
          >
            <HelpCircle className="w-4 h-4 text-emerald-400" /> User Guide
          </button>

          <div className="flex items-center gap-2 bg-slate-900/80 border border-slate-800 px-3 py-1.5 rounded-lg text-xs">
            <span className="text-slate-500 font-mono">Host:</span>
            <input
              type="text"
              value={backendURL}
              onChange={(e) => setBackendURL(e.target.value)}
              className="bg-transparent text-slate-200 border-none outline-none font-mono w-36 text-xs text-right focus:text-emerald-400 transition"
            />
          </div>

          {isConnecting ? (
            <div className="flex items-center gap-1.5 text-yellow-500 text-xs font-semibold bg-yellow-500/10 border border-yellow-500/20 py-1.5 px-3 rounded-full">
              <RefreshCw className="w-3.5 h-3.5 animate-spin" /> Connecting
            </div>
          ) : isConnected ? (
            <div className="flex items-center gap-1.5 text-emerald-500 text-xs font-semibold bg-emerald-500/10 border border-emerald-500/20 py-1.5 px-3 rounded-full">
              <span className="w-2 h-2 rounded-full bg-emerald-500 animate-ping inline-block" /> Live
            </div>
          ) : (
            <div className="flex items-center gap-1.5 text-rose-500 text-xs font-semibold bg-rose-500/10 border border-rose-500/20 py-1.5 px-3 rounded-full">
              <AlertTriangle className="w-3.5 h-3.5" /> Disconnected
            </div>
          )}
        </div>
      </header>

      {/* Connection Offline Banner */}
      {!isConnected && !isConnecting && (
        <div className="m-6 p-5 bg-rose-950/20 border border-rose-900/40 rounded-2xl flex flex-col md:flex-row md:items-center justify-between gap-4">
          <div>
            <h3 className="text-sm font-bold text-rose-400 flex items-center gap-2">
              <AlertTriangle className="w-5 h-5" /> Connection Failed
            </h3>
            <p className="text-xs text-slate-400 mt-1">
              Could not establish connection to the Go backend. Ensure the bot is running (`./trading_bot`) on your specified port.
            </p>
          </div>
          <button
            onClick={fetchData}
            className="self-start md:self-auto bg-slate-900 hover:bg-slate-800 border border-slate-800 text-slate-200 text-xs py-2 px-4 rounded-xl flex items-center gap-2 transition active:scale-95"
          >
            <RefreshCw className="w-3.5 h-3.5" /> Retry Connection
          </button>
        </div>
      )}

      {/* Main Grid Content */}
      <main className="flex-1 p-6 grid grid-cols-1 xl:grid-cols-4 gap-6">
        {/* Left column - Workspace (Candlestick chart, manual execution, positions, history) */}
        <div className="xl:col-span-3 flex flex-col gap-6">
          {/* KPIs Bar */}
          <div className="grid grid-cols-1 md:grid-cols-4 gap-4">
            <div className="bg-slate-900/40 border border-slate-900 p-5 rounded-2xl flex items-center justify-between">
              <div>
                <p className="text-[10px] text-slate-500 font-bold uppercase tracking-wider">Account Balance</p>
                <h3 className="text-xl font-black text-slate-100 mt-1 font-mono">
                  ${status?.balance ? status.balance.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }) : "100,000.00"}
                </h3>
              </div>
              <div className="text-emerald-500 bg-emerald-500/10 p-2.5 rounded-xl border border-emerald-500/20">
                <DollarSign className="w-5 h-5" />
              </div>
            </div>

            <div className="bg-slate-900/40 border border-slate-900 p-5 rounded-2xl flex items-center justify-between">
              <div>
                <p className="text-[10px] text-slate-500 font-bold uppercase tracking-wider">Account Equity</p>
                <h3 className="text-xl font-black text-slate-100 mt-1 font-mono">
                  ${status?.equity ? status.equity.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }) : "100,000.00"}
                </h3>
              </div>
              <div className="text-indigo-500 bg-indigo-500/10 p-2.5 rounded-xl border border-indigo-500/20">
                <ShieldCheck className="w-5 h-5" />
              </div>
            </div>

            <div className="bg-slate-900/40 border border-slate-900 p-5 rounded-2xl flex items-center justify-between">
              <div>
                <p className="text-[10px] text-slate-500 font-bold uppercase tracking-wider">Active Position</p>
                <h3 className="text-lg font-black text-slate-100 mt-1 font-mono">
                  {positions.length > 0 ? (
                    <span className={positions[0].units > 0 ? "text-emerald-400" : "text-rose-400"}>
                      {positions[0].units > 0 ? "LONG" : "SHORT"} ({Math.abs(positions[0].units).toLocaleString()} Units)
                    </span>
                  ) : (
                    <span className="text-slate-400">FLAT</span>
                  )}
                </h3>
              </div>
              <div className="text-teal-500 bg-teal-500/10 p-2.5 rounded-xl border border-teal-500/20">
                <Layers className="w-5 h-5" />
              </div>
            </div>

            <div className="bg-slate-900/40 border border-slate-900 p-5 rounded-2xl flex items-center justify-between">
              <div>
                <p className="text-[10px] text-slate-500 font-bold uppercase tracking-wider">Bot Environment</p>
                <h3 className="text-sm font-black text-slate-100 mt-2 font-mono flex items-center gap-1.5">
                  <span className="inline-block w-2.5 h-2.5 rounded-full bg-indigo-500 shadow-lg shadow-indigo-500/50" />
                  {status?.environment === "real" ? "OANDA DEMO/REAL" : "LOCAL SIMULATION"}
                </h3>
              </div>
              <div className="text-purple-500 bg-purple-500/10 p-2.5 rounded-xl border border-purple-500/20">
                <Sliders className="w-5 h-5" />
              </div>
            </div>
          </div>

          {/* SVG Candlestick Chart */}
          <div className="bg-slate-900/20 border border-slate-900 p-5 rounded-2xl flex flex-col gap-4">
            <div className="flex items-center justify-between">
              <h2 className="text-sm font-bold text-slate-200 flex items-center gap-2">
                <Activity className="w-4 h-4 text-emerald-400" /> Market Charting
              </h2>
              {positions.length > 0 && (
                <div className="text-xs bg-slate-900/80 px-3 py-1 rounded-full border border-slate-800 text-slate-400">
                  Open Entry: <span className="font-mono text-slate-200">{positions[0].open_price.toFixed(5)}</span>
                </div>
              )}
            </div>
            {renderChart()}
          </div>

          {/* Open Positions & Manual Order Grid */}
          <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
            {/* Active Positions */}
            <div className="lg:col-span-2 bg-slate-900/20 border border-slate-900 p-5 rounded-2xl flex flex-col">
              <h3 className="text-sm font-bold text-slate-200 flex items-center gap-2 mb-4 border-b border-slate-900 pb-3">
                <Layers className="w-4 h-4 text-emerald-400" /> Open Position
              </h3>

              {positions.length === 0 ? (
                <div className="flex-1 flex flex-col items-center justify-center py-8 text-slate-500">
                  <span className="w-8 h-8 rounded-full border border-dashed border-slate-700 flex items-center justify-center text-slate-600 mb-2">∅</span>
                  No open positions currently.
                </div>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-xs text-left">
                    <thead>
                      <tr className="text-slate-500 border-b border-slate-900/80">
                        <th className="pb-2">Instrument</th>
                        <th className="pb-2">Type</th>
                        <th className="pb-2 font-mono">Units</th>
                        <th className="pb-2 font-mono">Entry Price</th>
                        <th className="pb-2 font-mono">SL / TP</th>
                        <th className="pb-2 text-right">Actions</th>
                      </tr>
                    </thead>
                    <tbody>
                      {positions.map((pos) => {
                        const isLong = pos.units > 0;
                        const isMatchingInstrument = pos.instrument === status?.runner_config.instrument;
                        const currentPrice = (isMatchingInstrument && candles.length > 0) ? candles[candles.length - 1].close : pos.open_price;
                        const floatingPnl = (currentPrice - pos.open_price) * pos.units;

                        return (
                          <tr key={pos.id} className="border-b border-slate-900/40 hover:bg-slate-900/10">
                            <td className="py-3 font-semibold">{pos.instrument}</td>
                            <td className="py-3">
                              <span className={`px-2 py-0.5 rounded-full text-[10px] font-bold ${isLong ? "bg-emerald-500/10 text-emerald-400 border border-emerald-500/20" : "bg-rose-500/10 text-rose-400 border border-rose-500/20"}`}>
                                {isLong ? "BUY/LONG" : "SELL/SHORT"}
                              </span>
                            </td>
                            <td className="py-3 font-mono">{Math.abs(pos.units).toLocaleString()}</td>
                            <td className="py-3 font-mono">{pos.open_price.toFixed(5)}</td>
                            <td className="py-3 font-mono text-slate-400 text-[10px]">
                              SL: {pos.stop_loss > 0 ? pos.stop_loss.toFixed(5) : "None"}<br />
                              TP: {pos.take_profit > 0 ? pos.take_profit.toFixed(5) : "None"}
                            </td>
                            <td className="py-3 text-right">
                              <div className="flex items-center justify-end gap-3">
                                <span className={`font-mono font-bold ${floatingPnl >= 0 ? "text-emerald-400" : "text-rose-400"}`}>
                                  {floatingPnl >= 0 ? "+" : ""}${floatingPnl.toFixed(2)}
                                </span>
                                <button
                                  onClick={() => handleClosePosition(pos.id, currentPrice)}
                                  className="bg-rose-500/10 hover:bg-rose-500 text-rose-400 hover:text-white border border-rose-500/20 text-[10px] font-semibold py-1 px-3 rounded-lg transition active:scale-95"
                                >
                                  Close
                                </button>
                              </div>
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </div>

            {/* Manual Trading Widget */}
            <div className="bg-slate-900/20 border border-slate-900 p-5 rounded-2xl flex flex-col">
              <h3 className="text-sm font-bold text-slate-200 flex items-center gap-2 mb-4 border-b border-slate-900 pb-3">
                <Sliders className="w-4 h-4 text-emerald-400" /> Manual Trading
              </h3>

              <div className="flex flex-col gap-3.5 flex-1">
                <div>
                  <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">Trade Size (Units)</label>
                  <input
                    type="number"
                    value={manualUnits}
                    onChange={(e) => setManualUnits(Math.max(1, Number(e.target.value)))}
                    className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono focus:border-emerald-500 focus:outline-none transition"
                  />
                </div>

                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">Stop Loss</label>
                    <input
                      type="number"
                      step="0.0001"
                      value={manualStopLoss}
                      onChange={(e) => setManualStopLoss(Number(e.target.value))}
                      placeholder="0.0000"
                      className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono focus:border-emerald-500 focus:outline-none transition"
                    />
                  </div>
                  <div>
                    <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">Take Profit</label>
                    <input
                      type="number"
                      step="0.0001"
                      value={manualTakeProfit}
                      onChange={(e) => setManualTakeProfit(Number(e.target.value))}
                      placeholder="0.0000"
                      className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono focus:border-emerald-500 focus:outline-none transition"
                    />
                  </div>
                </div>

                <div className="grid grid-cols-2 gap-3 mt-2">
                  <button
                    onClick={() => handleManualTrade("BUY")}
                    className="bg-emerald-500/10 hover:bg-emerald-500 text-emerald-400 hover:text-slate-950 border border-emerald-500/20 text-xs font-bold py-2.5 px-4 rounded-xl transition active:scale-95 flex items-center justify-center gap-1.5 shadow-lg shadow-emerald-500/5 hover:shadow-emerald-500/10"
                  >
                    <ArrowUpRight className="w-4 h-4" /> Market Buy
                  </button>
                  <button
                    onClick={() => handleManualTrade("SELL")}
                    className="bg-rose-500/10 hover:bg-rose-500 text-rose-400 hover:text-white border border-rose-500/20 text-xs font-bold py-2.5 px-4 rounded-xl transition active:scale-95 flex items-center justify-center gap-1.5 shadow-lg shadow-rose-500/5 hover:shadow-rose-500/10"
                  >
                    <ArrowDownRight className="w-4 h-4" /> Market Sell
                  </button>
                </div>
              </div>
            </div>
          </div>

          {/* Historical Closed Trades */}
          <div className="bg-slate-900/20 border border-slate-900 p-5 rounded-2xl">
            <h3 className="text-sm font-bold text-slate-200 flex items-center gap-2 mb-4 border-b border-slate-900 pb-3">
              <History className="w-4 h-4 text-emerald-400" /> Trading History
            </h3>

            {trades.length === 0 ? (
              <div className="text-center py-8 text-slate-500 text-xs">
                No past transactions recorded in the SQLite database.
              </div>
            ) : (
              <div className="overflow-x-auto max-h-64 overflow-y-auto">
                <table className="w-full text-xs text-left">
                  <thead>
                    <tr className="text-slate-500 border-b border-slate-900/80">
                      <th className="pb-2">Timestamp</th>
                      <th className="pb-2">Type</th>
                      <th className="pb-2">Instrument</th>
                      <th className="pb-2 font-mono">Price</th>
                      <th className="pb-2 font-mono">Units</th>
                      <th className="pb-2 text-right">Realized PnL</th>
                    </tr>
                  </thead>
                  <tbody>
                    {trades.map((tx) => {
                      const isClose = tx.type === "CLOSE";
                      return (
                        <tr key={tx.id} className="border-b border-slate-900/40 hover:bg-slate-900/10">
                          <td className="py-2.5 text-slate-400 font-mono text-[10px]">
                            {new Date(tx.timestamp).toLocaleString()}
                          </td>
                          <td className="py-2.5">
                            <span className={`px-2 py-0.5 rounded text-[9px] font-bold ${tx.type === "BUY" ? "bg-emerald-500/10 text-emerald-400 border border-emerald-500/20" : tx.type === "SELL" ? "bg-rose-500/10 text-rose-400 border border-rose-500/20" : "bg-slate-800 text-slate-300"}`}>
                              {tx.type}
                            </span>
                          </td>
                          <td className="py-2.5 font-semibold text-slate-300">{tx.instrument}</td>
                          <td className="py-2.5 font-mono text-slate-300">{tx.price.toFixed(5)}</td>
                          <td className="py-2.5 font-mono text-slate-400">{Math.abs(tx.units).toLocaleString()}</td>
                          <td className="py-2.5 text-right font-mono font-bold">
                            {isClose ? (
                              <span className={tx.realized_pnl >= 0 ? "text-emerald-400" : "text-rose-400"}>
                                {tx.realized_pnl >= 0 ? "+" : ""}${tx.realized_pnl.toFixed(2)}
                              </span>
                            ) : (
                              <span className="text-slate-500">-</span>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </div>

        {/* Right column - Controls & Config, AI cache, Logs console */}
        <div className="flex flex-col gap-6">
          {/* Strategy Orchestrator Control Panel */}
          <div className="bg-slate-900/20 border border-slate-900 p-5 rounded-2xl flex flex-col">
            <div className="flex items-center justify-between mb-4 border-b border-slate-900 pb-3">
              <h3 className="text-sm font-bold text-slate-200 flex items-center gap-2">
                <Settings className="w-4 h-4 text-emerald-400" /> Strategy Config
              </h3>
              <div className="flex items-center gap-2">
                {tradingEnabled ? (
                  <button
                    onClick={() => handleToggleTrading(false)}
                    className="bg-emerald-500/10 hover:bg-emerald-600 hover:text-white text-emerald-400 border border-emerald-500/20 text-[10px] font-bold py-1 px-3 rounded-full flex items-center gap-1 transition active:scale-95"
                  >
                    <Play className="w-2.5 h-2.5 fill-current" /> Running
                  </button>
                ) : (
                  <button
                    onClick={() => handleToggleTrading(true)}
                    className="bg-slate-900 hover:bg-slate-800 text-slate-400 border border-slate-800 text-[10px] font-bold py-1 px-3 rounded-full flex items-center gap-1 transition active:scale-95"
                  >
                    <Pause className="w-2.5 h-2.5 fill-current" /> Paused
                  </button>
                )}
              </div>
            </div>

            <form onSubmit={handleUpdateConfig} className="flex flex-col gap-3.5">
              <div>
                <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">Asset Instrument</label>
                <input
                  type="text"
                  value={instrument}
                  onChange={(e) => setInstrument(e.target.value.toUpperCase())}
                  className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono text-slate-200 focus:border-emerald-500 focus:outline-none transition"
                />
              </div>

              <div>
                <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">Allora AI Topic ID</label>
                <input
                  type="number"
                  value={alloraTopicID}
                  onChange={(e) => setAlloraTopicID(Number(e.target.value))}
                  className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono text-slate-200 focus:border-emerald-500 focus:outline-none transition"
                />
              </div>

              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">Risk % per Trade</label>
                  <input
                    type="number"
                    step="0.1"
                    value={riskPercent}
                    onChange={(e) => setRiskPercent(Number(e.target.value))}
                    className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono text-slate-200 focus:border-emerald-500 focus:outline-none transition"
                  />
                </div>
                <div>
                  <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">ATR SL Multiplier</label>
                  <input
                    type="number"
                    step="0.1"
                    value={atrMultiplier}
                    onChange={(e) => setAtrMultiplier(Number(e.target.value))}
                    className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono text-slate-200 focus:border-emerald-500 focus:outline-none transition"
                  />
                </div>
              </div>

              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">Fast EMA Period</label>
                  <input
                    type="number"
                    value={emaFast}
                    onChange={(e) => setEmaFast(Number(e.target.value))}
                    className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono text-slate-200 focus:border-emerald-500 focus:outline-none transition"
                  />
                </div>
                <div>
                  <label className="text-[10px] text-slate-400 block font-bold uppercase tracking-wider mb-1.5">Slow EMA Period</label>
                  <input
                    type="number"
                    value={emaSlow}
                    onChange={(e) => setEmaSlow(Number(e.target.value))}
                    className="w-full bg-slate-950 border border-slate-900 rounded-xl px-3 py-2 text-xs font-mono text-slate-200 focus:border-emerald-500 focus:outline-none transition"
                  />
                </div>
              </div>

              <div className="flex items-center justify-between border-t border-slate-900 pt-3.5 mt-1">
                <span className="text-xs text-slate-300 font-semibold flex items-center gap-1.5">
                  <Cpu className="w-3.5 h-3.5 text-emerald-400" /> Use Allora AI
                </span>
                <input
                  type="checkbox"
                  checked={useAllora}
                  onChange={(e) => setUseAllora(e.target.checked)}
                  className="rounded bg-slate-950 border-slate-900 text-emerald-500 focus:ring-emerald-500 w-4 h-4"
                />
              </div>

              <button
                type="submit"
                disabled={!status}
                className="w-full bg-gradient-to-r from-emerald-500 to-teal-400 hover:from-emerald-400 hover:to-teal-300 text-slate-950 font-bold text-xs py-3 px-4 rounded-xl transition active:scale-98 disabled:opacity-50 mt-2 shadow-lg shadow-emerald-500/10"
              >
                Apply Parameters
              </button>
            </form>
          </div>

          {/* Allora AI Forecast Inference Cache */}
          <div className="bg-slate-900/20 border border-slate-900 p-5 rounded-2xl flex flex-col">
            <h3 className="text-sm font-bold text-slate-200 flex items-center gap-2 mb-3 border-b border-slate-900 pb-3">
              <Cpu className="w-4 h-4 text-emerald-400" /> Allora Network Inferences
            </h3>

            {inferences.length === 0 ? (
              <div className="text-center py-8 text-slate-500 text-xs">
                No cached inferences found. Enable "Use Allora AI".
              </div>
            ) : (
              <div className="overflow-y-auto max-h-56">
                <div className="flex flex-col gap-2.5">
                  {inferences.map((inf, idx) => {
                    const isBullish = inf.parsed_value > 0;
                    return (
                      <div key={idx} className="bg-slate-950/60 p-3 rounded-xl border border-slate-900 flex items-center justify-between text-xs hover:border-slate-800 transition">
                        <div>
                          <div className="flex items-center gap-1.5 text-slate-400 text-[10px]">
                            <span>Topic {inf.topic_id}</span>
                            <span>•</span>
                            <span className="font-mono">Block #{inf.block_height}</span>
                          </div>
                          <div className="font-mono text-slate-200 font-bold mt-1 text-[10px] max-w-[140px] truncate">
                            Value: {inf.combined_value}
                          </div>
                        </div>

                        <div className="text-right">
                          <span className={`px-2 py-0.5 rounded-full text-[9px] font-bold inline-flex items-center gap-0.5 ${isBullish ? "bg-emerald-500/10 text-emerald-400 border border-emerald-500/20" : "bg-rose-500/10 text-rose-400 border border-rose-500/20"}`}>
                            {isBullish ? <TrendingUp className="w-2.5 h-2.5" /> : <TrendingDown className="w-2.5 h-2.5" />}
                            {isBullish ? "BULLISH" : "BEARISH"}
                          </span>
                          <span className="block text-[8px] text-slate-500 font-mono mt-1">
                            {new Date(inf.timestamp).toLocaleTimeString()}
                          </span>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </div>
            )}
          </div>

          {/* System Terminal Console */}
          <div className="bg-slate-900/20 border border-slate-900 p-5 rounded-2xl flex flex-col flex-1 min-h-[300px]">
            <h3 className="text-sm font-bold text-slate-200 flex items-center gap-2 mb-3 border-b border-slate-900 pb-3">
              <Terminal className="w-4 h-4 text-emerald-400" /> Live Console Logs
            </h3>

            <div
              ref={terminalContainerRef}
              className="flex-1 bg-slate-950/90 rounded-xl p-3.5 border border-slate-900 font-mono text-[10px] overflow-y-auto max-h-[360px] leading-relaxed shadow-inner"
            >
              {logs.length === 0 ? (
                <div className="text-slate-600 italic">No system events logged yet.</div>
              ) : (
                <div className="flex flex-col gap-1.5">
                  {logs.map((log) => {
                    const isError = log.level === "ERROR";
                    const isWarn = log.level === "WARN";
                    const color = isError ? "text-rose-400" : isWarn ? "text-yellow-500" : "text-cyan-400";
                    return (
                      <div key={log.id} className="border-b border-slate-900/20 pb-1 text-slate-300">
                        <span className="text-slate-500">[{new Date(log.timestamp).toLocaleTimeString()}]</span>{" "}
                        <span className={`font-bold ${color}`}>[{log.level}]</span>{" "}
                        <span className="text-slate-200">{log.message}</span>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          </div>
        </div>
      </main>
      {/* User Guide Overlay Modal */}
      {showManual && (
        <div className="fixed inset-0 bg-slate-950/80 backdrop-blur-md z-[100] flex items-center justify-center p-4">
          <div className="bg-slate-900 border border-slate-800 rounded-3xl w-full max-w-2xl shadow-2xl overflow-hidden flex flex-col max-h-[85vh]">
            {/* Modal Header */}
            <div className="p-6 border-b border-slate-800/60 flex items-center justify-between bg-slate-900/40">
              <div className="flex items-center gap-2.5">
                <div className="bg-emerald-500/10 p-2 rounded-xl border border-emerald-500/20 text-emerald-400">
                  <HelpCircle className="w-5 h-5" />
                </div>
                <div>
                  <h2 className="text-base font-bold text-slate-100">TIQ AI Platform User Guide</h2>
                  <p className="text-[10px] text-slate-500">Learn how to configure, monitor, and trade with your agent</p>
                </div>
              </div>
              <button
                onClick={() => setShowManual(false)}
                className="p-2 hover:bg-slate-800/80 rounded-xl text-slate-400 hover:text-slate-200 transition active:scale-95"
              >
                <X className="w-5 h-5" />
              </button>
            </div>

            {/* Modal Body */}
            <div className="p-6 overflow-y-auto space-y-6 text-xs leading-relaxed text-slate-300">
              <div>
                <h3 className="text-xs font-bold text-emerald-400 uppercase tracking-wider mb-2">1. Operating Modes</h3>
                <p className="mb-2">
                  The bot supports two distinct operational frameworks, defined at startup via the backend environment:
                </p>
                <ul className="list-disc pl-4 space-y-1.5 text-slate-400">
                  <li>
                    <strong className="text-slate-200">Local Simulation:</strong> A risk-free paper trading environment. Uses an internal matching engine to track virtual balances and execute manual/automated orders. Trades, account balances, and historical performance are saved to the local SQLite database.
                  </li>
                  <li>
                    <strong className="text-slate-200">Oanda Live/Demo:</strong> Connects directly to Oanda broker endpoints. Orders, trades, and account balances are fetched and executed directly against Oanda’s REST v20 servers.
                  </li>
                </ul>
              </div>

              <div>
                <h3 className="text-xs font-bold text-emerald-400 uppercase tracking-wider mb-2">2. Automated Strategy Logic</h3>
                <p className="mb-2">
                  The automated trading loop runs in the background at a fixed 60-second ticker. It performs analysis and makes decisions based on:
                </p>
                <ul className="list-disc pl-4 space-y-1.5 text-slate-400">
                  <li>
                    <strong className="text-slate-200">Technical Crossover:</strong> Looks for Fast EMA crossing over/under Slow EMA (indicates trend direction) filtered by the Relative Strength Index (RSI) to avoid entering oversold or overbought states.
                  </li>
                  <li>
                    <strong className="text-slate-200">Allora AI Signals (Optional):</strong> Queries the Allora Network for topic-specific price inferences. If <code className="text-emerald-400 font-mono">Use Allora AI</code> is checked, trades will only execute if the AI forecast aligns with the technical trend (e.g. bullish technicals + positive Allora inference).
                  </li>
                  <li>
                    <strong className="text-slate-200">Position Management:</strong> Reverses positions immediately if an opposite crossover signal occurs.
                  </li>
                </ul>
              </div>

              <div>
                <h3 className="text-xs font-bold text-emerald-400 uppercase tracking-wider mb-2">3. Stop Loss (SL) & Take Profit (TP)</h3>
                <p className="mb-2">
                  Risk management is structured differently for automated and manual orders:
                </p>
                <ul className="list-disc pl-4 space-y-1.5 text-slate-400">
                  <li>
                    <strong className="text-slate-200">Automated Trades:</strong> Uses dynamic ATR-based boundaries. Stop Loss is set at <code className="text-indigo-400 font-mono">ATR * SL Multiplier</code>, and Take Profit is set at <code className="text-indigo-400 font-mono">ATR * TP Multiplier</code>. In simulation mode, the engine updates asset prices on every tick and automatically closes positions if the price hits these SL/TP barriers.
                  </li>
                  <li>
                    <strong className="text-slate-200">Manual Trades:</strong> You can enter custom SL/TP price levels directly in the Manual Trading ticket form. Setting these to <code className="text-slate-400 font-mono">0</code> disables SL/TP management for that order.
                  </li>
                </ul>
              </div>

              <div>
                <h3 className="text-xs font-bold text-emerald-400 uppercase tracking-wider mb-2">4. 24/7 Trading (BTC/USD) vs Closed Markets</h3>
                <p className="mb-2">
                  Forex assets (like EUR/USD) only trade from Sunday evening through Friday afternoon. If the market is closed:
                </p>
                <ul className="list-disc pl-4 space-y-1.5 text-slate-400">
                  <li>
                    Candle prices remain frozen at Friday's closing value.
                  </li>
                  <li>
                    Floating P&L will remain static at <code className="text-slate-400 font-mono">+$0.00</code>.
                  </li>
                  <li>
                    To see active price movements, floating P&L fluctuations, and automatic SL/TP triggers in real-time, configure the system to trade <code className="text-emerald-400 font-mono">BTC_USD</code> (which trades continuously 24/7).
                  </li>
                </ul>
              </div>
            </div>

            {/* Modal Footer */}
            <div className="p-5 border-t border-slate-800/60 flex justify-end bg-slate-900/40">
              <button
                onClick={() => setShowManual(false)}
                className="bg-gradient-to-r from-emerald-500 to-teal-400 hover:from-emerald-400 hover:to-teal-300 text-slate-950 font-bold text-xs py-2 px-6 rounded-xl transition active:scale-95 shadow-lg shadow-emerald-500/10"
              >
                Got It
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

// EMA calculator helper function for frontend SVG drawing
function calculateEmaArray(closes: number[], period: number): number[] {
	const ema = new Array(closes.length).fill(0);
	if (closes.length < period) return ema;

	let sum = 0;
	for (let i = 0; i < period; i++) {
		sum += closes[i];
	}
	ema[period - 1] = sum / period;

	const multiplier = 2 / (period + 1);
	for (let i = period; i < closes.length; i++) {
		ema[i] = (closes[i] * multiplier) + (ema[i - 1] * (1 - multiplier));
	}
	return ema;
}
