# LATPC plots

Generate the Phase 10 figures from the collected timing CSV:

```bash
python3 -m pip install -r tools/latpc_plot/requirements.txt
MPLCONFIGDIR=/private/tmp/latpc-matplotlib \
  python3 tools/latpc_plot/plot.py
```

The script uses only Matplotlib and NumPy (no seaborn) and writes PNG/PDF
pairs plus `manifest.md` under `out/latpc/evaluation/figures/`.
