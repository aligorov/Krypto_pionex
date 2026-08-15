import React, { useEffect, useRef, useState } from 'react';
import { createChart, CandlestickSeries, IChartApi, ISeriesApi, CandlestickData, Time, LineStyle } from 'lightweight-charts';

export interface GridLevel {
  price: number;
  side: 'buy' | 'sell';
}

export interface CandlestickChartProps {
  symbol: string;
  lowerPrice?: number;
  upperPrice?: number;
  currentPrice?: number;
  stopLoss?: number;
  gridLevels?: GridLevel[];
  gridCount?: number;
  direction?: string;
  onClose?: () => void;
}

export const CandlestickChart: React.FC<CandlestickChartProps> = ({
  symbol,
  lowerPrice,
  upperPrice,
  currentPrice,
  stopLoss,
  gridLevels,
  gridCount,
  direction = 'NEUTRAL',
  onClose,
}) => {
  const chartContainerRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const candleSeriesRef = useRef<ISeriesApi<'Candlestick'> | null>(null);
  const [interval, setInterval] = useState<string>('15M');
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<string | null>(null);
  const [lastCandle, setLastCandle] = useState<{ open: number; high: number; low: number; close: number } | null>(null);

  useEffect(() => {
    let isMounted = true;

    async function loadCandles() {
      setLoading(true);
      setError(null);
      try {
        const resp = await fetch(`/api/market/candles?symbol=${encodeURIComponent(symbol)}&interval=${interval}&limit=150`, {
          credentials: 'include',
        });
        if (!resp.ok) {
          throw new Error(`HTTP ${resp.status}`);
        }
        const data = await resp.json();
        if (!isMounted) return;

        if (data.candles && data.candles.length > 0) {
          const chartData: CandlestickData<Time>[] = data.candles.map((c: any) => ({
            time: c.time as Time,
            open: c.open,
            high: c.high,
            low: c.low,
            close: c.close,
          }));

          chartData.sort((a, b) => (Number(a.time) - Number(b.time)));

          if (candleSeriesRef.current) {
            candleSeriesRef.current.setData(chartData);
            chartRef.current?.timeScale().fitContent();
            const last = chartData[chartData.length - 1];
            setLastCandle({ open: last.open, high: last.high, low: last.low, close: last.close });
          }
        } else if (data.error) {
          setError(data.error);
        }
      } catch (err: any) {
        if (isMounted) setError(err.message || 'Ошибка загрузки свечей');
      } finally {
        if (isMounted) setLoading(false);
      }
    }

    if (!chartContainerRef.current) return;

    const chart = createChart(chartContainerRef.current, {
      layout: {
        background: { color: '#0b0f17' },
        textColor: '#94a3b8',
        fontSize: 12,
        fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
      },
      grid: {
        vertLines: { color: '#1e293b' },
        horzLines: { color: '#1e293b' },
      },
      crosshair: {
        vertLine: { color: '#38bdf8', width: 1, style: LineStyle.Dashed },
        horzLine: { color: '#38bdf8', width: 1, style: LineStyle.Dashed },
      },
      rightPriceScale: {
        borderColor: '#334155',
        autoScale: true,
      },
      timeScale: {
        borderColor: '#334155',
        timeVisible: true,
        secondsVisible: false,
      },
    });
    chartRef.current = chart;

    const candleSeries = chart.addSeries(CandlestickSeries, {
      upColor: '#10b981',
      downColor: '#ef4444',
      borderVisible: false,
      wickUpColor: '#10b981',
      wickDownColor: '#ef4444',
    });
    candleSeriesRef.current = candleSeries;

    // Draw Upper Bound
    if (upperPrice && upperPrice > 0) {
      candleSeries.createPriceLine({
        price: upperPrice,
        color: '#f59e0b',
        lineWidth: 2,
        lineStyle: LineStyle.Dashed,
        axisLabelVisible: true,
        title: `ВЕРХ [${upperPrice}]`,
      });
    }

    // Draw Lower Bound
    if (lowerPrice && lowerPrice > 0) {
      candleSeries.createPriceLine({
        price: lowerPrice,
        color: '#f59e0b',
        lineWidth: 2,
        lineStyle: LineStyle.Dashed,
        axisLabelVisible: true,
        title: `НИЗ [${lowerPrice}]`,
      });
    }

    // Draw Stop Loss
    if (stopLoss && stopLoss > 0) {
      candleSeries.createPriceLine({
        price: stopLoss,
        color: '#ef4444',
        lineWidth: 2,
        lineStyle: LineStyle.Solid,
        axisLabelVisible: true,
        title: `СТОП-ЛОСС [${stopLoss}]`,
      });
    }

    // Draw Current Price Line
    if (currentPrice && currentPrice > 0) {
      candleSeries.createPriceLine({
        price: currentPrice,
        color: '#38bdf8',
        lineWidth: 1,
        lineStyle: LineStyle.Solid,
        axisLabelVisible: true,
        title: `ТЕКУЩАЯ [${currentPrice}]`,
      });
    }

    // Generate and Draw Full Grid Levels Ladder
    const effectiveGridCount = gridCount && gridCount >= 2 ? gridCount : 20;
    const computedLevels: GridLevel[] = [];

    if (gridLevels && gridLevels.length > 0) {
      computedLevels.push(...gridLevels);
    } else if (lowerPrice && upperPrice && upperPrice > lowerPrice) {
      const ratio = Math.pow(upperPrice / lowerPrice, 1 / effectiveGridCount);
      const mid = currentPrice && currentPrice > 0 ? currentPrice : (lowerPrice + upperPrice) / 2;

      for (let i = 1; i < effectiveGridCount; i++) {
        const lvlPrice = lowerPrice * Math.pow(ratio, i);
        computedLevels.push({
          price: lvlPrice,
          side: lvlPrice <= mid ? 'buy' : 'sell',
        });
      }
    }

    // Draw all intermediate grid levels
    computedLevels.forEach((lvl) => {
      candleSeries.createPriceLine({
        price: lvl.price,
        color: lvl.side === 'buy' ? 'rgba(34, 197, 94, 0.7)' : 'rgba(244, 63, 94, 0.7)',
        lineWidth: 1,
        lineStyle: LineStyle.Dotted,
        axisLabelVisible: true,
        title: lvl.side === 'buy' ? 'BUY' : 'SELL',
      });
    });

    const handleResize = () => {
      if (chartContainerRef.current && chartRef.current) {
        chartRef.current.applyOptions({
          width: chartContainerRef.current.clientWidth,
          height: chartContainerRef.current.clientHeight,
        });
      }
    };
    window.addEventListener('resize', handleResize);

    loadCandles();

    return () => {
      isMounted = false;
      window.removeEventListener('resize', handleResize);
      chart.remove();
    };
  }, [symbol, interval, lowerPrice, upperPrice, stopLoss, currentPrice, gridCount, gridLevels]);

  return (
    <div style={{
      display: 'flex',
      flexDirection: 'column',
      height: '100%',
      backgroundColor: '#0b0f17',
      borderRadius: '8px',
      overflow: 'hidden',
      border: '1px solid #1e293b',
    }}>
      {/* Header bar */}
      <div style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        padding: '12px 16px',
        borderBottom: '1px solid #1e293b',
        backgroundColor: '#0f172a',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
          <span style={{ fontSize: '16px', fontWeight: 'bold', color: '#f8fafc' }}>
            📊 {symbol}
          </span>
          <span style={{
            fontSize: '11px',
            padding: '2px 8px',
            borderRadius: '4px',
            backgroundColor: direction === 'LONG' ? 'rgba(16,185,129,0.2)' : direction === 'SHORT' ? 'rgba(239,68,68,0.2)' : 'rgba(148,163,184,0.2)',
            color: direction === 'LONG' ? '#10b981' : direction === 'SHORT' ? '#ef4444' : '#94a3b8',
            fontWeight: '600',
          }}>
            {direction}
          </span>
          {gridCount && (
            <span style={{ fontSize: '12px', color: '#64748b' }}>
              • {gridCount} уровней сетки
            </span>
          )}
          {lastCandle && (
            <div style={{ display: 'flex', gap: '8px', fontSize: '11px', color: '#94a3b8' }}>
              <span>O: <b style={{ color: '#f1f5f9' }}>{lastCandle.open}</b></span>
              <span>H: <b style={{ color: '#10b981' }}>{lastCandle.high}</b></span>
              <span>L: <b style={{ color: '#ef4444' }}>{lastCandle.low}</b></span>
              <span>C: <b style={{ color: lastCandle.close >= lastCandle.open ? '#10b981' : '#ef4444' }}>{lastCandle.close}</b></span>
            </div>
          )}
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <div style={{ display: 'flex', backgroundColor: '#1e293b', borderRadius: '4px', padding: '2px' }}>
            {['1M', '5M', '15M', '1H', '4H', '1D'].map((tf) => (
              <button
                key={tf}
                onClick={() => setInterval(tf)}
                style={{
                  background: interval === tf ? '#38bdf8' : 'transparent',
                  color: interval === tf ? '#0f172a' : '#94a3b8',
                  border: 'none',
                  borderRadius: '3px',
                  padding: '4px 8px',
                  fontSize: '11px',
                  fontWeight: interval === tf ? 'bold' : 'normal',
                  cursor: 'pointer',
                  transition: 'all 0.15s ease',
                }}
              >
                {tf}
              </button>
            ))}
          </div>

          {onClose && (
            <button
              onClick={onClose}
              style={{
                background: '#1e293b',
                border: 'none',
                color: '#cbd5e1',
                borderRadius: '4px',
                padding: '4px 10px',
                cursor: 'pointer',
                fontSize: '14px',
              }}
            >
              ✕
            </button>
          )}
        </div>
      </div>

      <div style={{ position: 'relative', flex: 1, minHeight: '400px' }}>
        {loading && (
          <div style={{
            position: 'absolute',
            inset: 0,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            backgroundColor: 'rgba(11, 15, 23, 0.7)',
            zIndex: 10,
            color: '#38bdf8',
            fontSize: '13px',
          }}>
            Загрузка свечей {symbol}...
          </div>
        )}
        {error && (
          <div style={{
            position: 'absolute',
            inset: 0,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            backgroundColor: 'rgba(11, 15, 23, 0.85)',
            zIndex: 10,
            color: '#ef4444',
            fontSize: '13px',
            padding: '16px',
            textAlign: 'center',
          }}>
            ⚠️ {error}
          </div>
        )}
        <div ref={chartContainerRef} style={{ width: '100%', height: '100%' }} />
      </div>

      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: '16px',
        padding: '8px 16px',
        borderTop: '1px solid #1e293b',
        backgroundColor: '#090d16',
        fontSize: '11px',
        color: '#94a3b8',
        flexWrap: 'wrap',
      }}>
        <span style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
          <span style={{ display: 'inline-block', width: '10px', height: '2px', backgroundColor: '#f59e0b' }}></span>
          Границы ({lowerPrice || '—'} – {upperPrice || '—'})
        </span>
        <span style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
          <span style={{ display: 'inline-block', width: '10px', height: '2px', backgroundColor: '#22c55e' }}></span>
          🟢 Buy ордера сетки
        </span>
        <span style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
          <span style={{ display: 'inline-block', width: '10px', height: '2px', backgroundColor: '#f43f5e' }}></span>
          🔴 Sell ордера сетки
        </span>
        {stopLoss && (
          <span style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
            <span style={{ display: 'inline-block', width: '10px', height: '2px', backgroundColor: '#ef4444' }}></span>
            Стоп-лосс ({stopLoss})
          </span>
        )}
      </div>
    </div>
  );
};
