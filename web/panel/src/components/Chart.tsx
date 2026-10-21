import { useEffect, useRef } from 'react';
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';

export interface Series {
  label: string;
  color: string;
  values: (number | null)[];
  fill?: boolean;
  format?: (v: number) => string;
}

const two = (n: number) => String(n).padStart(2, '0');
const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

function timeLabel(secs: number, span: number, withSeconds = false): string {
  const d = new Date(secs * 1000);
  const hm = two(d.getHours()) + ':' + two(d.getMinutes()) + (withSeconds ? ':' + two(d.getSeconds()) : '');
  const day = d.getDate() + ' ' + months[d.getMonth()];
  if (span > 3 * 86400) return day;
  if (span > 86400) return day + ' ' + hm;
  return hm;
}

/** A time-series chart (uPlot). times are unix seconds. */
export function Chart({ times, series, height = 220, yFormat }: {
  times: number[]; series: Series[]; height?: number; yFormat?: (v: number) => string;
}) {
  const box = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);

  useEffect(() => {
    if (!box.current) return;
    const css = getComputedStyle(document.documentElement);
    const grid = css.getPropertyValue('--line').trim() || '#2c333d';
    const text = css.getPropertyValue('--text-3').trim() || '#7c8794';

    const opts: uPlot.Options = {
      width: box.current.clientWidth,
      height,
      cursor: { points: { size: 6 } },
      legend: { show: series.length > 1 },
      scales: { x: { time: true } },
      axes: [
        {
          stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid }, font: '12px "IBM Plex Sans"', space: 70,
          values: (u, vals) => {
            const span = (u.scales.x.max ?? 0) - (u.scales.x.min ?? 0);
            return vals.map((v) => timeLabel(v, span, span < 600));
          },
        },
        {
          stroke: text, grid: { stroke: grid, width: 1 }, ticks: { show: false }, size: 64,
          font: '12px "IBM Plex Sans"', values: (_u, vals) => vals.map((v) => (yFormat ? yFormat(v) : String(v))),
        },
      ],
      series: [
        { value: (u: uPlot, v: number | null) => (v == null ? '—' : timeLabel(v, (u.scales.x.max ?? 0) - (u.scales.x.min ?? 0), true)) },
        ...series.map((s) => ({
          label: s.label,
          stroke: s.color,
          width: 1.6,
          fill: s.fill ? s.color + '22' : undefined,
          points: { show: false },
          value: (_u: uPlot, v: number | null) => (v == null ? '—' : s.format ? s.format(v) : String(v)),
        })),
      ],
    };
    const u = new uPlot(opts, [times, ...series.map((s) => s.values)] as uPlot.AlignedData, box.current);
    plot.current = u;

    const ro = new ResizeObserver(() => {
      if (box.current) u.setSize({ width: box.current.clientWidth, height });
    });
    ro.observe(box.current);
    return () => {
      ro.disconnect();
      u.destroy();
      plot.current = null;
    };
    // Recreate when the shape of the chart changes; data updates use setData.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [series.length, height, series.map((s) => s.label).join()]);

  useEffect(() => {
    plot.current?.setData([times, ...series.map((s) => s.values)] as uPlot.AlignedData);
  }, [times, series]);

  return <div className="chart" ref={box} />;
}
