// Soporte UDP embebido (protocolo udpgw de badvpn) -- antes corría como un
// proceso Go separado (github.com/mukswilly/udpgw) escuchando en
// 127.0.0.1:7300, al que GPM simplemente reenviaba el canal SSH como un
// direct-tcpip más. Esta versión reimplementa el lado servidor del
// protocolo DENTRO de GPM: cuando un canal direct-tcpip apunta a la
// dirección configurada como "udpgw" (por default 127.0.0.1:7300), GPM lo
// atiende directo en vez de abrir una conexión TCP real a nadie -- un
// proceso menos que desplegar/monitorear, y GPM ve/cuenta el tráfico UDP
// igual que cualquier otro canal.
//
// Formato de cada mensaje (idéntico en los dos sentidos, ver
// https://github.com/ambrop72/badvpn y el server real en Go
// github.com/mukswilly/udpgw/udpgw.go, de donde se tomó el protocolo):
//
//	[2B size LE][1B flags][2B connID LE][4B o 16B ip][2B puerto BE][payload]
package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// udpgwFlushInterval: el canal udpgw dura TODA la sesión VPN (un solo
// canal multiplexa todos los flujos UDP, a diferencia de TCP donde cada
// conexión es su propio canal que abre y cierra seguido). Sin un flush
// periódico, el consumo UDP -- que en un teléfono moderno puede ser la
// mayoría del tráfico real, vía QUIC -- no le llega al panel hasta que el
// usuario desconecta del todo. Como el chequeo de cuota del panel
// (`u+d < transfer_enable` en cada pull de /UniProxy/user) es la ÚNICA
// aplicación de límite que existe, sin esto un usuario conectado por días
// podría consumir muy por encima de su plan sin que el panel lo detecte.
const udpgwFlushInterval = 30 * time.Second

const (
	udpgwFlagKeepalive = 1 << 0
	udpgwFlagRebind    = 1 << 1
	udpgwFlagDNS       = 1 << 2
	udpgwFlagIPv6      = 1 << 3

	udpgwMaxPayloadSize = 32768
	udpgwMaxMessageSize = 23 + udpgwMaxPayloadSize
)

// readUdpgwMessage lee UN mensaje udpgw real (no de keepalive -- esos se
// consumen y se sigue leyendo el siguiente) del reader dado, usando buffer
// como scratch space reusable.
func readUdpgwMessage(reader io.Reader, buffer []byte) (connID uint16, remoteIP []byte, remotePort uint16, payload []byte, err error) {
	for {
		if _, err = io.ReadFull(reader, buffer[0:2]); err != nil {
			return
		}
		size := binary.LittleEndian.Uint16(buffer[0:2])
		if size < 3 || int(size) > len(buffer)-2 {
			err = fmt.Errorf("udpgw: tamaño de mensaje inválido (%d)", size)
			return
		}
		if _, err = io.ReadFull(reader, buffer[2:2+size]); err != nil {
			return
		}
		flags := buffer[2]
		connID = binary.LittleEndian.Uint16(buffer[3:5])

		if flags&udpgwFlagKeepalive != 0 {
			continue // sin dirección/payload -- seguir leyendo el siguiente mensaje
		}

		var ipLen, packetStart, packetEnd int
		if flags&udpgwFlagIPv6 != 0 {
			if size < 21 {
				err = fmt.Errorf("udpgw: mensaje IPv6 truncado (size=%d)", size)
				return
			}
			ipLen, packetStart, packetEnd = 16, 23, 23+int(size)-21
		} else {
			if size < 9 {
				err = fmt.Errorf("udpgw: mensaje IPv4 truncado (size=%d)", size)
				return
			}
			ipLen, packetStart, packetEnd = 4, 11, 11+int(size)-9
		}
		remoteIP = append([]byte(nil), buffer[5:5+ipLen]...)
		remotePort = binary.BigEndian.Uint16(buffer[5+ipLen : 5+ipLen+2])
		payload = append([]byte(nil), buffer[packetStart:packetEnd]...)
		return
	}
}

// writeUdpgwMessage arma y escribe un mensaje udpgw. w se protege con mu
// porque varios goroutines de relayDownstream (una por cada connID activo)
// escriben concurrentemente sobre el MISMO canal SSH.
func writeUdpgwMessage(w io.Writer, mu *sync.Mutex, connID uint16, remoteIP []byte, remotePort uint16, payload []byte) error {
	var flags byte
	if len(remoteIP) == 16 {
		flags = udpgwFlagIPv6
	}
	preambleSize := 7 + len(remoteIP)
	msg := make([]byte, preambleSize+len(payload))
	size := uint16(preambleSize-2) + uint16(len(payload))
	binary.LittleEndian.PutUint16(msg[0:2], size)
	msg[2] = flags
	binary.LittleEndian.PutUint16(msg[3:5], connID)
	copy(msg[5:5+len(remoteIP)], remoteIP)
	binary.BigEndian.PutUint16(msg[5+len(remoteIP):7+len(remoteIP)], remotePort)
	copy(msg[preambleSize:], payload)

	mu.Lock()
	defer mu.Unlock()
	_, err := w.Write(msg)
	return err
}

type udpgwPortForward struct {
	connID     uint16
	remoteIP   []byte
	remotePort uint16
	conn       *net.UDPConn
}

// handleUdpgwChannel atiende UN canal direct-tcpip que el cliente abrió
// hacia la dirección "virtual" de udpgw como si fuera el multiplexor real
// -- multiplexa varios destinos UDP (identificados por connID) sobre ese
// único canal, igual que hacía el proceso separado.
func handleUdpgwChannel(channel ssh.Channel, uuid, name string, usage *Usage) {
	defer channel.Close()

	var writeMu sync.Mutex
	var up, down int64

	// Flush periódico mientras el canal sigue vivo (ver udpgwFlushInterval)
	// -- no basta con reportar solo al cerrar, este canal dura toda la
	// sesión VPN.
	flushDone := make(chan struct{})
	var flushWG sync.WaitGroup
	flushWG.Add(1)
	go func() {
		defer flushWG.Done()
		flushUdpgwUsage(uuid, usage, &up, &down, flushDone)
	}()

	var pfMu sync.Mutex
	portForwards := map[uint16]*udpgwPortForward{}
	var relayWG sync.WaitGroup

	buffer := make([]byte, udpgwMaxMessageSize)
	for {
		connID, remoteIP, remotePort, payload, err := readUdpgwMessage(channel, buffer)
		if err != nil {
			break
		}

		pfMu.Lock()
		pf := portForwards[connID]
		if pf != nil && (!bytes.Equal(pf.remoteIP, remoteIP) || pf.remotePort != remotePort) {
			// Cliente reusa este connID para un destino distinto -- cerrar
			// el forward viejo, el goroutine de relayDownstream se limpia
			// solo al ver el UDPConn cerrado.
			pf.conn.Close()
			delete(portForwards, connID)
			pf = nil
		}
		if pf == nil {
			udpConn, derr := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IP(remoteIP), Port: int(remotePort)})
			if derr != nil {
				pfMu.Unlock()
				continue
			}
			pf = &udpgwPortForward{connID: connID, remoteIP: remoteIP, remotePort: remotePort, conn: udpConn}
			portForwards[connID] = pf

			relayWG.Add(1)
			go func() {
				defer relayWG.Done()
				downBuf := make([]byte, 65535)
				for {
					n, rerr := udpConn.Read(downBuf)
					if n > 0 {
						atomic.AddInt64(&down, int64(n))
						if werr := writeUdpgwMessage(channel, &writeMu, connID, remoteIP, remotePort, downBuf[:n]); werr != nil {
							break
						}
					}
					if rerr != nil {
						break
					}
				}
				pfMu.Lock()
				if portForwards[connID] == pf {
					delete(portForwards, connID)
				}
				pfMu.Unlock()
				udpConn.Close()
			}()
		}
		pfMu.Unlock()

		if len(payload) > 0 {
			if _, werr := pf.conn.Write(payload); werr == nil {
				atomic.AddInt64(&up, int64(len(payload)))
			}
		}
	}

	pfMu.Lock()
	for _, pf := range portForwards {
		pf.conn.Close()
	}
	pfMu.Unlock()
	relayWG.Wait()

	close(flushDone)
	flushWG.Wait() // asegura que el tramo final (desde el último tick) ya se reportó

	log.Printf("[%s] udpgw embebido cerrado, subida total=%dB bajada total=%dB", name, atomic.LoadInt64(&up), atomic.LoadInt64(&down))
}

// flushUdpgwUsage reporta el consumo acumulado en incrementos (no solo al
// final) mientras el canal udpgw sigue abierto. up/down son punteros a los
// contadores ATÓMICOS acumulativos que va incrementando handleUdpgwChannel
// -- aquí solo se leen y se reporta el DELTA desde el último flush (Usage.Add
// suma, no reemplaza, así que reportar el total de nuevo en cada tick
// duplicaría el consumo).
func flushUdpgwUsage(uuid string, usage *Usage, up, down *int64, done <-chan struct{}) {
	ticker := time.NewTicker(udpgwFlushInterval)
	defer ticker.Stop()

	var lastUp, lastDown int64
	flush := func() {
		curUp := atomic.LoadInt64(up)
		curDown := atomic.LoadInt64(down)
		deltaUp := curUp - lastUp
		deltaDown := curDown - lastDown
		if deltaUp > 0 || deltaDown > 0 {
			usage.Add(uuid, deltaUp, deltaDown)
			log.Printf("udpgw: flush parcial uuid=%s subida=%dB bajada=%dB", uuid, deltaUp, deltaDown)
		}
		lastUp, lastDown = curUp, curDown
	}

	for {
		select {
		case <-ticker.C:
			flush()
		case <-done:
			flush() // último tramo, sin esperar al siguiente tick
			return
		}
	}
}
