package internal

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Implementation of regular DeviceMemoryState", func() {

	var regularDMS DeviceMemoryState

	BeforeEach(func() {
		regularDMS = newDeviceRegularMemoryState(12)
		regularDMS.setStorageSize(0x1_0000_0000)
		regularDMS.setInitialAddress(0x0_0000_1000)
	})

	It("should retain recycled PAddrs without dense frame backing", func() {
		regularDMS.addSinglePAddr(0x0_0000_1000)
		regularDMS.addSinglePAddr(0x0_0000_2000)
		regularDMS.addSinglePAddr(0x0_0000_3000)
		regularDMS.addSinglePAddr(0x0_0000_4000)

		rDMS := regularDMS.(*deviceMemoryStateImpl)

		Expect(rDMS.recycledPAddrs).To(HaveLen(4))
		Expect(rDMS.nextPAddr).To(Equal(uint64(0x0_0000_1000)))
	})

	It("should get sequential PAddrs", func() {
		addr1 := regularDMS.popNextAvailablePAddrs()
		addr2 := regularDMS.popNextAvailablePAddrs()

		Expect(addr1).To(Equal(uint64(0x0_0000_1000)))
		Expect(addr2).To(Equal(uint64(0x0_0000_2000)))
		rDMS := regularDMS.(*deviceMemoryStateImpl)
		Expect(rDMS.recycledPAddrs).To(BeEmpty())
		Expect(rDMS.nextPAddr).To(Equal(uint64(0x0_0000_3000)))
	})

	It("should allocate multiple PAddrs", func() {
		addrs := regularDMS.allocateMultiplePages(3)

		Expect(addrs).To(HaveLen(3))
		Expect(addrs[0]).To(Equal(uint64(0x0_0000_1000)))
		Expect(addrs[1]).To(Equal(uint64(0x0_0000_2000)))
		Expect(addrs[2]).To(Equal(uint64(0x0_0000_3000)))

		rDMS := regularDMS.(*deviceMemoryStateImpl)
		Expect(rDMS.recycledPAddrs).To(BeEmpty())
		Expect(rDMS.nextPAddr).To(Equal(uint64(0x0_0000_4000)))
	})

	It("should have no available PAddrs", func() {
		regularDMS.setStorageSize(0)
		regularDMS.setInitialAddress(0x0_0000_1000)
		ok := regularDMS.noAvailablePAddrs()
		Expect(ok).To(BeTrue())

		regularDMS.addSinglePAddr(0x0_0000_1000)
		ok = regularDMS.noAvailablePAddrs()
		Expect(ok).To(BeFalse())
	})

})
