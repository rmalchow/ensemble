"""Design data: USB-PD 20V -> 5V buck -> 3V3 LDO, ESP32-S3, PCM5102A DAC, TPA3118D2 amp.

Single source of truth for both the schematic and PCB generators.

Each component: ref -> dict(
    sym   = (lib, symbol_name)        # 'ensemble' lib = custom embedded symbol
    val   = value string
    fp    = 'FootprintLib:FootprintName'
    conn  = {pin_number_or_name: net}  # name matches ALL pins with that name
    sch   = (x, y) schematic position, mm
    pcb   = (x, y, rot) board position, mm
)
Pins not listed in conn get a no-connect marker.
"""

PROJECT = 'ensemble-amp'
TITLE = 'ESP32-S3 I2S Audio Amplifier  (USB-PD 20V / PCM5102A / TPA3118D2)'

# nets that get a power symbol when pin direction allows (GND down, rails up)
PWR_SYMBOL_NETS = {'GND': 'power:GND', '+5V': 'power:+5V',
                   '+3V3': 'power:+3V3', 'VBUS': 'power:VBUS'}

R_ = ('Device', 'R')
C_ = ('Device', 'C')
CP_ = ('Device', 'C_Polarized')
L_ = ('Device', 'L')
LED_ = ('Device', 'LED')

FP_R = 'Resistor_SMD:R_0603_1608Metric'
FP_C = 'Capacitor_SMD:C_0603_1608Metric'
FP_C8 = 'Capacitor_SMD:C_0805_2012Metric'
FP_C12 = 'Capacitor_SMD:C_1210_3225Metric'
FP_CP = 'Capacitor_THT:CP_Radial_D10.0mm_P5.00mm'
FP_CP35 = 'Capacitor_THT:CP_Radial_D12.5mm_P5.00mm'
FP_LBIG = 'Inductor_SMD:L_12x12mm_H8mm'
FP_LED = 'LED_SMD:LED_0805_2012Metric'
FP_TERM = 'TerminalBlock_Phoenix:TerminalBlock_Phoenix_MKDS-3-2-5.08_1x02_P5.08mm_Horizontal'
FP_SW = 'Button_Switch_SMD:SW_SPST_B3S-1000'


def _r(ref, val, n1, n2, sch, pcb, fp=FP_R):
    return ref, dict(sym=R_, val=val, fp=fp, conn={'1': n1, '2': n2}, sch=sch, pcb=pcb)


def _c(ref, val, n1, n2, sch, pcb, fp=FP_C):
    return ref, dict(sym=C_, val=val, fp=fp, conn={'1': n1, '2': n2}, sch=sch, pcb=pcb)


COMPONENTS = dict([
    # ---------------- USB-PD power input (20 V / 3 A, GPIO mode) ----------------
    # HUSB238A autonomous sink: VSET 19.1k = 20 V, ISET 21k = 3 A (Tables 8/9).
    # Falls back down the source's PDO list (15 V, 12 V, 9 V, 5 V) when the
    # charger can't do 20 V/3 A. ADDR/ORIENT floating = GPIO mode.
    ('U7', dict(sym=('Interface_USB', 'HUSB238A-xxxxx-QN16R'), val='HUSB238A',
                fp='Package_DFN_QFN:WQFN-16-1EP_3x3mm_P0.5mm_EP1.75x1.75mm',
                conn={'VBUS': 'VBUS', 'CC1': 'CC1', 'CC2': 'CC2',
                      'VDD': 'PD_VDD', 'GATE': 'PD_GATE',
                      'SDA/SNK_VSET': 'PD_VSET', 'SCL/SNK_ISET': 'PD_ISET',
                      'FAULT/OUT2': 'PD_FAULT', 'FLGIN': 'GND',
                      '~{EN}': 'GND', 'GND': 'GND'},
                sch=(53.34, 50.8), pcb=(58, 78, 0))),
    _r('R3', '19.1k 1%', 'PD_VSET', 'GND', (119.38, 45.72), (54, 72, 0)),   # 20 V
    _r('R4', '21k 1%', 'PD_ISET', 'GND', (127.0, 45.72), (54, 75, 0)),     # 3 A
    _c('C55', '1u', 'PD_VDD', 'GND', (149.86, 45.72), (63, 75, 0)),
    # VBUS load switch: PMOS driven by HUSB238A GATE (open drain), soft start
    ('Q1', dict(sym=('Transistor_FET', 'SUD45P03-09'), val='SUD45P03-09',
                fp='Package_TO_SOT_SMD:TO-252-2',
                conn={'G': 'PD_GATE', 'D': 'VAMP', 'S': 'VBUS'},
                sch=(86.36, 33.02), pcb=(58, 58, 180))),
    _r('R1', '100k', 'VBUS', 'PD_GATE', (96.52, 33.02), (64, 55, 0)),
    ('D5', dict(sym=('Device', 'D_Zener'), val='BZT52C10 (Vgs clamp)',
                fp='Diode_SMD:D_SOD-123',
                conn={'A': 'PD_GATE', 'K': 'VBUS'}, sch=(106.68, 33.02), pcb=(64, 58, 0))),
    _c('C54', '10n', 'PD_GATE', 'GND', (116.84, 33.02), (64, 61, 0)),
    ('D1', dict(sym=('Device', 'D_TVS'), val='SMAJ24A', fp='Diode_SMD:D_SMA',
                conn={'1': 'VBUS', '2': 'GND'}, sch=(124.46, 33.02), pcb=(68, 75, 0))),
    # bulk on the switched amp rail
    ('C1', dict(sym=CP_, val='470u/35V', fp=FP_CP35, conn={'1': 'VAMP', '2': 'GND'},
                sch=(91.44, 45.72), pcb=(70, 57, 0))),
    ('C2', dict(sym=CP_, val='470u/35V', fp=FP_CP35, conn={'1': 'VAMP', '2': 'GND'},
                sch=(101.6, 45.72), pcb=(84, 57, 0))),
    _c('C3', '100n', 'VAMP', 'GND', (111.76, 45.72), (108, 57, 90)),
    # rail voltage sense divider -> ESP32 ADC (20 V -> 1.82 V)
    _r('R22', '100k', 'VAMP', 'VAMP_SENSE', (266.7, 116.84), (76, 100, 0)),
    _r('R23', '10k', 'VAMP_SENSE', 'GND', (274.32, 116.84), (80, 100, 0)),
    _c('C56', '100n', 'VAMP_SENSE', 'GND', (281.94, 116.84), (76, 103, 0)),

    # ---------------- 5 V buck (AP63205, fixed 5V, 2A) ----------------
    ('U1', dict(sym=('Regulator_Switching', 'AP63205WU'), val='AP63205WU',
                fp='Package_TO_SOT_SMD:TSOT-23-6',
                conn={'IN': 'VAMP', 'EN': 'VAMP', 'GND': 'GND',
                      'SW': 'SW5V', 'BST': 'BST5V', 'FB': '+5V'},
                sch=(55.88, 76.2), pcb=(122, 57, 0))),
    _c('C4', '10u/35V', 'VAMP', 'GND', (33.02, 78.74), (118, 61, 0), FP_C12),
    _c('C5', '100n', 'VAMP', 'GND', (40.64, 78.74), (122, 61, 0)),
    _c('C6', '100n', 'BST5V', 'SW5V', (78.74, 71.12), (126, 61, 0)),
    ('L1', dict(sym=L_, val='6.8u', fp='Inductor_SMD:L_Coilcraft_XAL5030-XXX',
                conn={'1': 'SW5V', '2': '+5V'}, sch=(88.9, 76.2), pcb=(132, 58, 0))),
    _c('C7', '22u', '+5V', 'GND', (99.06, 78.74), (137, 56, 0), FP_C8),
    _c('C8', '22u', '+5V', 'GND', (106.68, 78.74), (137, 60, 0), FP_C8),

    # ---------------- 3.3 V LDO (low dropout: works from 5 V USB fallback) ----------------
    ('U2', dict(sym=('Regulator_Linear', 'AP2112K-3.3'), val='AP2112K-3.3',
                fp='Package_TO_SOT_SMD:SOT-23-5',
                conn={'VIN': '+5V', 'EN': '+5V', 'VOUT': '+3V3', 'GND': 'GND'},
                sch=(55.88, 101.6), pcb=(147, 57, 0))),
    _c('C9', '10u', '+5V', 'GND', (33.02, 104.14), (142, 61, 0), FP_C8),
    _c('C10', '22u', '+3V3', 'GND', (71.12, 104.14), (152, 61, 0), FP_C8),
    _c('C11', '100n', '+3V3', 'GND', (78.74, 104.14), (155, 57, 90)),
    ('FB1', dict(sym=('Device', 'FerriteBead'), val='600R@100MHz',
                 fp='Inductor_SMD:L_0805_2012Metric',
                 conn={'1': '+3V3', '2': '3V3A'}, sch=(91.44, 101.6), pcb=(147, 64, 0))),
    _c('C12', '10u', '3V3A', 'GND', (101.6, 104.14), (143, 67, 0), FP_C8),
    _c('C13', '100n', '3V3A', 'GND', (109.22, 104.14), (147, 67, 0)),

    # ---------------- USB-C (native USB, programming + power) ----------------
    ('J2', dict(sym=('Connector', 'USB_C_Receptacle_USB2.0_16P'), val='USB-C',
                fp='Connector_USB:USB_C_Receptacle_HRO_TYPE-C-31-M-12',
                conn={'VBUS': 'VBUS', 'GND': 'GND', 'SHIELD': 'GND',
                      'CC1': 'CC1', 'CC2': 'CC2', 'D+': 'USB_DP', 'D-': 'USB_DN'},
                sch=(45.72, 157.48), pcb=(53, 85, 270))),
    ('U6', dict(sym=('Power_Protection', 'USBLC6-2SC6'), val='USBLC6-2SC6',
                fp='Package_TO_SOT_SMD:SOT-23-6',
                conn={'I/O1': 'USB_DN', 'I/O2': 'USB_DP', 'GND': 'GND', 'VBUS': '+5V'},
                sch=(96.52, 154.94), pcb=(61, 85, 90))),
    _c('C18', '10u/35V', 'VBUS', 'GND', (114.3, 157.48), (63, 80, 0), FP_C12),

    # ---------------- ESP32-S3-WROOM-1 ----------------
    ('U3', dict(sym=('RF_Module', 'ESP32-S3-WROOM-1'), val='ESP32-S3-WROOM-1-N8R8',
                fp='RF_Module:ESP32-S3-WROOM-1',
                conn={'GND': 'GND', '3V3': '+3V3', 'EN': 'ESP_EN', 'IO0': 'ESP_IO0',
                      'USB_D-': 'USB_DN', 'USB_D+': 'USB_DP',
                      'IO15': 'I2S_BCLK', 'IO16': 'I2S_LRCK', 'IO17': 'I2S_DOUT',
                      'IO8': 'I2C_SDA', 'IO9': 'I2C_SCL',
                      'IO4': 'ENC_A', 'IO5': 'ENC_B', 'IO6': 'ENC_SW',
                      'IO10': 'AMP_SDZ_G', 'IO11': 'AMP_MUTE_G',
                      'IO12': 'AMP_FAULT', 'IO13': 'DAC_XSMT',
                      'IO47': 'LED1', 'IO48': 'LED2',
                      'IO7': 'VAMP_SENSE', 'IO14': 'PD_FAULT',
                      'TXD0': 'UART_TX', 'RXD0': 'UART_RX',
                      'IO1': 'SPARE1', 'IO2': 'SPARE2'},
                sch=(165.1, 96.52), pcb=(70, 121, 180))),
    _r('R2', '10k', '+3V3', 'ESP_EN', (134.62, 48.26), (80, 106, 0)),
    _c('C14', '1u', 'ESP_EN', 'GND', (142.24, 48.26), (84, 106, 0)),
    ('SW1', dict(sym=('Switch', 'SW_Push'), val='RESET', fp=FP_SW,
                 conn={'1': 'ESP_EN', '2': 'GND'}, sch=(132.08, 60.96), pcb=(85, 113, 0))),
    ('SW2', dict(sym=('Switch', 'SW_Push'), val='BOOT', fp=FP_SW,
                 conn={'1': 'ESP_IO0', '2': 'GND'}, sch=(132.08, 73.66), pcb=(85, 122, 0))),
    _c('C15', '100n', '+3V3', 'GND', (134.62, 147.32), (80, 130, 90)),
    _c('C16', '10u', '+3V3', 'GND', (142.24, 147.32), (84, 130, 90), FP_C8),
    _c('C17', '22u', '+3V3', 'GND', (149.86, 147.32), (88, 130, 90), FP_C8),
    ('J3', dict(sym=('Connector_Generic', 'Conn_01x06'), val='UART/DEBUG',
                fp='Connector_PinHeader_2.54mm:PinHeader_1x06_P2.54mm_Vertical',
                conn={'1': '+3V3', '2': 'GND', '3': 'UART_TX', '4': 'UART_RX',
                      '5': 'SPARE1', '6': 'SPARE2'},
                sch=(203.2, 154.94), pcb=(105, 131, 0))),

    # ---------------- PCM5102A I2S DAC ----------------
    ('U4', dict(sym=('Audio', 'PCM5102A'), val='PCM5102A', fp='Package_SO:TSSOP-20_4.4x6.5mm_P0.65mm',
                conn={'DVDD': '+3V3', 'DGND': 'GND', 'AVDD': '3V3A', 'AGND': 'GND',
                      'CPVDD': '3V3A', 'CPGND': 'GND',
                      'CAPP': 'CAPP', 'CAPM': 'CAPM', 'VNEG': 'VNEG', 'LDOO': 'DAC_LDO',
                      'BCK': 'I2S_BCLK', 'DIN': 'I2S_DOUT', 'LRCK': 'I2S_LRCK',
                      'SCK': 'GND', 'FMT': 'GND', 'DEMP': 'GND', 'FLT': 'GND',
                      'XSMT': 'DAC_XSMT',
                      'OUTL': 'DAC_OUTL', 'OUTR': 'DAC_OUTR'},
                sch=(309.88, 63.5), pcb=(102, 90, 0))),
    _c('C19', '100n', '+3V3', 'GND', (287.02, 99.06), (96, 84, 0)),
    _c('C20', '10u', '+3V3', 'GND', (294.64, 99.06), (96, 88, 0), FP_C8),
    _c('C21', '100n', '3V3A', 'GND', (302.26, 99.06), (108, 84, 0)),
    _c('C22', '10u', '3V3A', 'GND', (309.88, 99.06), (108, 88, 0), FP_C8),
    _c('C23', '100n', '3V3A', 'GND', (317.5, 99.06), (96, 92, 0)),
    _c('C24', '10u', '3V3A', 'GND', (325.12, 99.06), (108, 92, 0), FP_C8),
    _c('C25', '2.2u', 'CAPP', 'CAPM', (340.36, 55.88), (96, 96, 0), FP_C8),
    _c('C26', '2.2u', 'VNEG', 'GND', (347.98, 55.88), (108, 96, 0), FP_C8),
    _c('C27', '1u', 'DAC_LDO', 'GND', (355.6, 55.88), (102, 100, 0)),
    _r('R7', '10k', 'DAC_XSMT', 'GND', (363.22, 55.88), (97, 100, 0)),
    # DAC output RC low-pass + AC coupling into the amp
    _r('R8', '100', 'DAC_OUTL', 'DACL_F', (340.36, 73.66), (112, 84, 0)),
    _c('C28', '2.2n', 'DACL_F', 'GND', (347.98, 73.66), (116, 84, 0)),
    _c('C29', '1u', 'DACL_F', 'AMP_INL', (355.6, 73.66), (120, 84, 0)),
    _r('R9', '100', 'DAC_OUTR', 'DACR_F', (340.36, 88.9), (112, 96, 0)),
    _c('C30', '2.2n', 'DACR_F', 'GND', (347.98, 88.9), (116, 96, 0)),
    _c('C31', '1u', 'DACR_F', 'AMP_INR', (355.6, 88.9), (120, 96, 0)),

    # ---------------- TPA3118D2 Class-D amplifier ----------------
    ('U5', dict(sym=('ensemble', 'TPA3118D2'), val='TPA3118D2DAP',
                fp='Package_SO:HTSSOP-32-1EP_6.1x11mm_P0.65mm_EP5.2x11mm_Mask4.11x4.36mm_ThermalVias',
                conn={'MODSEL': 'GND', 'SDZ': 'AMP_SDZ', 'FAULTZ': 'AMP_FAULT',
                      'RINP': 'AMP_INR', 'RINN': 'AMP_RINN',
                      'LINP': 'AMP_INL', 'LINN': 'AMP_LINN',
                      'PLIMIT': 'GVDD', 'GVDD': 'GVDD', 'GAIN/SLV': 'AMP_GAIN',
                      'MUTE': 'AMP_MUTE', 'AM0': 'GND', 'AM1': 'GND', 'AM2': 'GND',
                      'GND': 'GND', 'PAD': 'GND', 'AVCC': 'VAMP', 'PVCC': 'VAMP',
                      'BSNL': 'BSNL', 'OUTNL': 'OUTNL', 'OUTPL': 'OUTPL', 'BSPL': 'BSPL',
                      'BSNR': 'BSNR', 'OUTNR': 'OUTNR', 'OUTPR': 'OUTPR', 'BSPR': 'BSPR'},
                sch=(309.88, 165.1), pcb=(130, 90, 0))),
    _r('R10', '100k', 'AMP_SDZ_G', 'AMP_SDZ', (243.84, 198.12), (124, 103, 0)),
    _r('R11', '100k', 'AMP_SDZ', 'GND', (251.46, 198.12), (128, 103, 0)),
    _r('R12', '100k', '+3V3', 'AMP_FAULT', (274.32, 198.12), (124, 106, 0)),
    _r('R13', '5k6', 'AMP_GAIN', 'GND', (281.94, 198.12), (128, 106, 0)),  # 20 dB, master
    _r('R14', '100k', 'AMP_MUTE_G', 'AMP_MUTE', (259.08, 198.12), (124, 109, 0)),
    _r('R15', '100k', 'AMP_MUTE', 'GND', (266.7, 198.12), (128, 109, 0)),
    _c('C32', '1u', 'AMP_RINN', 'GND', (289.56, 198.12), (124, 77, 0)),
    _c('C33', '1u', 'GVDD', 'GND', (304.8, 198.12), (124, 112, 0)),
    _c('C34', '1u', 'AMP_LINN', 'GND', (297.18, 198.12), (128, 77, 0)),
    _c('C35', '1u/35V', 'VAMP', 'GND', (312.42, 198.12), (136, 79, 0)),     # AVCC
    _c('C36', '100n', 'VAMP', 'GND', (320.04, 198.12), (136, 82, 0)),
    _c('C37', '100n', 'VAMP', 'GND', (327.66, 198.12), (136, 95, 0)),   # PVCC L
    _c('C38', '1u/35V', 'VAMP', 'GND', (335.28, 198.12), (136, 85, 0)),
    _c('C39', '100n', 'VAMP', 'GND', (342.9, 198.12), (136, 98, 0)),    # PVCC R
    _c('C40', '1u/35V', 'VAMP', 'GND', (350.52, 198.12), (136, 101, 0)),
    ('C41', dict(sym=CP_, val='470u/35V', fp=FP_CP35, conn={'1': 'VAMP', '2': 'GND'},
                 sch=(360.68, 198.12), pcb=(143, 78, 0))),
    ('C42', dict(sym=CP_, val='470u/35V', fp=FP_CP35, conn={'1': 'VAMP', '2': 'GND'},
                 sch=(370.84, 198.12), pcb=(143, 102, 0))),
    # bootstrap caps
    _c('C43', '220n', 'BSNL', 'OUTNL', (340.36, 144.78), (136, 88, 0)),
    _c('C44', '220n', 'BSPL', 'OUTPL', (347.98, 144.78), (136, 91, 0)),
    _c('C45', '220n', 'BSNR', 'OUTNR', (355.6, 144.78), (136, 104, 0)),
    _c('C46', '220n', 'BSPR', 'OUTPR', (363.22, 144.78), (136, 107, 0)),
    # output LC filters (BTL, 10uH + 680nF per phase)
    ('L2', dict(sym=L_, val='10u', fp=FP_LBIG, conn={'1': 'OUTPL', '2': 'SPKL_P'},
                sch=(340.36, 162.56), pcb=(146, 68, 0))),
    _c('C47', '680n/50V', 'SPKL_P', 'GND', (347.98, 162.56), (154, 68, 90), FP_C12),
    ('L3', dict(sym=L_, val='10u', fp=FP_LBIG, conn={'1': 'OUTNL', '2': 'SPKL_N'},
                sch=(355.6, 162.56), pcb=(146, 82, 0))),
    _c('C48', '680n/50V', 'SPKL_N', 'GND', (363.22, 162.56), (154, 82, 90), FP_C12),
    ('L4', dict(sym=L_, val='10u', fp=FP_LBIG, conn={'1': 'OUTPR', '2': 'SPKR_P'},
                sch=(340.36, 177.8), pcb=(146, 96, 0))),
    _c('C49', '680n/50V', 'SPKR_P', 'GND', (347.98, 177.8), (154, 96, 90), FP_C12),
    ('L5', dict(sym=L_, val='10u', fp=FP_LBIG, conn={'1': 'OUTNR', '2': 'SPKR_N'},
                sch=(355.6, 177.8), pcb=(146, 110, 0))),
    _c('C50', '680n/50V', 'SPKR_N', 'GND', (363.22, 177.8), (154, 110, 90), FP_C12),
    ('J4', dict(sym=('Connector', 'Screw_Terminal_01x02'), val='SPK_L', fp=FP_TERM,
                conn={'1': 'SPKL_P', '2': 'SPKL_N'}, sch=(381.0, 162.56), pcb=(155, 75, 90))),
    ('J5', dict(sym=('Connector', 'Screw_Terminal_01x02'), val='SPK_R', fp=FP_TERM,
                conn={'1': 'SPKR_P', '2': 'SPKR_N'}, sch=(381.0, 177.8), pcb=(155, 103, 90))),

    # ---------------- UI: encoder, OLED, LEDs ----------------
    ('ENC1', dict(sym=('Device', 'RotaryEncoder_Switch'), val='EC11E',
                  fp='Rotary_Encoder:RotaryEncoder_Alps_EC11E-Switch_Vertical_H20mm',
                  conn={'A': 'ENC_A', 'B': 'ENC_B', 'C': 'GND',
                        'S1': 'ENC_SW', 'S2': 'GND'},
                  sch=(228.6, 50.8), pcb=(95, 68, 0))),
    _r('R16', '10k', '+3V3', 'ENC_A', (243.84, 40.64), (88, 64, 0)),
    _r('R17', '10k', '+3V3', 'ENC_B', (251.46, 40.64), (88, 68, 0)),
    _r('R18', '10k', '+3V3', 'ENC_SW', (259.08, 40.64), (88, 72, 0)),
    _c('C51', '100n', 'ENC_A', 'GND', (243.84, 58.42), (91, 64, 0)),
    _c('C52', '100n', 'ENC_B', 'GND', (251.46, 58.42), (91, 68, 0)),
    _c('C53', '100n', 'ENC_SW', 'GND', (259.08, 58.42), (91, 72, 0)),
    ('J6', dict(sym=('Connector_Generic', 'Conn_01x04'), val='OLED_I2C',
                fp='Connector_PinHeader_2.54mm:PinHeader_1x04_P2.54mm_Vertical',
                conn={'1': 'GND', '2': '+3V3', '3': 'I2C_SCL', '4': 'I2C_SDA'},
                sch=(228.6, 88.9), pcb=(76, 73, 90))),
    _r('R5', '4k7', '+3V3', 'I2C_SDA', (246.38, 86.36), (81, 70, 0)),
    _r('R6', '4k7', '+3V3', 'I2C_SCL', (254.0, 86.36), (81, 74, 0)),
    ('D2', dict(sym=LED_, val='PWR_green', fp=FP_LED, conn={'A': 'PWR_LED', 'K': 'GND'},
                sch=(228.6, 116.84), pcb=(60, 104, 0))),
    _r('R19', '1k', '+3V3', 'PWR_LED', (243.84, 116.84), (64, 104, 0)),
    ('D3', dict(sym=LED_, val='LED_status1', fp=FP_LED, conn={'A': 'LED1_A', 'K': 'GND'},
                sch=(228.6, 132.08), pcb=(60, 108, 0))),
    _r('R20', '470', 'LED1', 'LED1_A', (243.84, 132.08), (64, 108, 0)),
    ('D4', dict(sym=LED_, val='LED_status2', fp=FP_LED, conn={'A': 'LED2_A', 'K': 'GND'},
                sch=(228.6, 147.32), pcb=(60, 112, 0))),
    _r('R21', '470', 'LED2', 'LED2_A', (243.84, 147.32), (64, 112, 0)),

    # ---------------- mounting holes ----------------
    ('H1', dict(sym=('Mechanical', 'MountingHole'), val='M3',
                fp='MountingHole:MountingHole_3.2mm_M3_DIN965', conn={},
                sch=(38.1, 264.16), pcb=(55, 55, 0))),
    ('H2', dict(sym=('Mechanical', 'MountingHole'), val='M3',
                fp='MountingHole:MountingHole_3.2mm_M3_DIN965', conn={},
                sch=(48.26, 264.16), pcb=(155, 55, 0))),
    ('H3', dict(sym=('Mechanical', 'MountingHole'), val='M3',
                fp='MountingHole:MountingHole_3.2mm_M3_DIN965', conn={},
                sch=(58.42, 264.16), pcb=(55, 130, 0))),
    ('H4', dict(sym=('Mechanical', 'MountingHole'), val='M3',
                fp='MountingHole:MountingHole_3.2mm_M3_DIN965', conn={},
                sch=(68.58, 264.16), pcb=(155, 130, 0))),
])

# board outline, mm
BOARD = dict(x1=50, y1=50, x2=160, y2=135)
